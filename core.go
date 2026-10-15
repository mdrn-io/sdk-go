package mdrn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	defaultTimeout   = 10 * time.Second
	defaultPartBytes = 16 * 1024 * 1024
	pollInterval     = time.Second
	waitLimit        = time.Minute
	conflictRetries  = 3
)

// Doer sends one HTTP request and answers its response: the transport of every request the
// client makes. *http.Client is one; so is anything that wraps one for tracing, or a transport
// of your own. A Doer must hand a 3xx back as it is instead of following it (the default
// client does, with CheckRedirect), so the SDK can refuse it and the token never leaves MDRN's
// origin. The per-request deadline is the request's context, so it holds for any Doer.
//
// In a Worker, product requests go through the MDRN_PRODUCTS service binding; a Doer backed by
// that binding is where a TinyGo build of this package would plug in. No such build exists yet.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// product is one of the client's products: where it is, the token for it, and (when it is not
// enabled) why not.
type product struct {
	url, token, unavailable string
}

// request is one product request before anything sends it: everything the wire needs, with no
// transport in it, so the request every SDK sends can be compared field by field.
type request struct {
	method, url                 string
	body                        []byte // nil when there is none
	token, project, environment string
	key                         string // Idempotency-Key, on a mutation
	ifMatch                     uint64 // If-Match, on a record update or delete
}

// build is the *http.Request of this request, with the headers every product request carries.
func (r request) build(ctx context.Context) (*http.Request, error) {
	var reader io.Reader
	if r.body != nil {
		reader = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, reader)
	if err != nil {
		return nil, argument("invalid request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("MDRN-Project", r.project)
	req.Header.Set("MDRN-Environment", r.environment)
	req.Header.Set("User-Agent", userAgent)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.ifMatch != 0 {
		req.Header.Set("If-Match", strconv.FormatUint(r.ifMatch, 10))
	}
	if r.key != "" {
		req.Header.Set("Idempotency-Key", r.key)
	}
	return req, nil
}

// response is one response as received: what decoding needs, with no transport in it.
type response struct {
	status int
	header http.Header
	body   []byte
}

// decode is the response's JSON body in out (nil out, a 204 or an empty body decode nothing),
// or the *Error it stands for: UNEXPECTED_REDIRECT for a 3xx, the envelope's code or HTTP_ERROR
// for any other failure.
func (r response) decode(out any) error {
	if r.status >= 300 && r.status < 400 {
		details, _ := json.Marshal(map[string]string{"location": r.header.Get("Location")})
		return &Error{Status: r.status, Code: CodeUnexpectedRedirect, Message: fmt.Sprintf("MDRN answered with a redirect (HTTP %d), which the SDK does not follow", r.status), Details: details}
	}
	if r.status < 200 || r.status >= 300 {
		return apiError(r.status, r.body)
	}
	if out == nil || r.status == http.StatusNoContent || len(r.body) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.body, out); err != nil {
		return &Error{Status: r.status, Code: CodeHTTPError, Message: fmt.Sprintf("MDRN answered HTTP %d with a body that is not the expected JSON: %v", r.status, err), cause: err}
	}
	return nil
}

// encode is v as JSON, as the wire gets it: no HTML escaping, no trailing newline. A value that
// cannot be encoded is an ArgumentError.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, argument("the value cannot be encoded as JSON: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// exchange sends one request through doer under the client's deadline and reads the whole
// response. A context already cancelled sends nothing and answers the cancellation error; a
// cancellation during the request does the same; the deadline is TIMEOUT and any other
// transport failure NETWORK_ERROR.
func (c *Client) exchange(ctx context.Context, doer Doer, build func(context.Context) (*http.Request, error)) (*response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timed, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := build(timed)
	if err != nil {
		return nil, err
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, timed, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, c.transportError(ctx, timed, err)
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

func (c *Client) transportError(ctx, timed context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(timed.Err(), context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: fmt.Sprintf("MDRN did not answer within %s", c.timeout), cause: err}
	}
	return &Error{Code: CodeNetworkError, Message: "MDRN could not be reached: " + err.Error(), cause: err}
}

// call is one request to a product: the product's URL and token, the project and environment,
// the write headers when given, and the JSON answer in out.
func (c *Client) call(ctx context.Context, p *product, method, path string, body any, key string, ifMatch uint64, out any) error {
	if p.url == "" {
		return &Error{Code: CodeProductNotEnabled, Message: p.unavailable}
	}
	var raw []byte
	if body != nil {
		var err error
		if raw, err = encode(body); err != nil {
			return err
		}
	}
	r := request{method: method, url: p.url + path, body: raw, token: p.token, project: c.Project, environment: c.Environment, key: key, ifMatch: ifMatch}
	resp, err := c.exchange(ctx, c.http, r.build)
	if err != nil {
		return err
	}
	return resp.decode(out)
}

// read is a GET that answers found=false, without an error, when there is no such thing.
func (c *Client) read(ctx context.Context, p *product, path string, out any) (bool, error) {
	err := c.call(ctx, p, http.MethodGet, path, nil, "", 0, out)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// wait pauses for the poll interval, or until the context ends.
func (c *Client) wait(ctx context.Context) error {
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// PageOptions selects a page of a list: at most Limit items (the server's default when 0)
// from Cursor (the previous page's NextCursor; the first page when empty). Both are sent as
// given: the server, not the SDK, decides what a valid limit is.
type PageOptions struct {
	Limit  int
	Cursor string
}

// query is the options' query string, "" when they are empty.
func (o PageOptions) query() string {
	q := ""
	if o.Limit != 0 {
		q += "limit=" + strconv.Itoa(o.Limit)
	}
	if o.Cursor != "" {
		if q != "" {
			q += "&"
		}
		q += "cursor=" + url.QueryEscape(o.Cursor)
	}
	if q == "" {
		return ""
	}
	return "?" + q
}

// Page is one page of a list and the cursor of the next, "" after the last.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor"`
}

// segment is one path segment, escaped.
func segment(value string) string { return url.PathEscape(value) }
