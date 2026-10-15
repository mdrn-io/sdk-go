package mdrn_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	mdrn "github.com/mdrn-io/sdk-go"
)

// A request as the fake server saw it.
type seen struct {
	Method, Path string
	Header       http.Header
	Body         string
}

// fake is an MDRN on 127.0.0.1 whose answers a test scripts, and the requests it received.
type fake struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	seen    []seen
	handler func(w http.ResponseWriter, r *http.Request, body string)
}

func newFake(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body string)) *fake {
	t.Helper()
	f := &fake{t: t, handler: handler}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seen{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: string(raw)})
		f.mu.Unlock()
		f.handler(w, r, string(raw))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fake) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.seen...)
}

func (f *fake) count() int { return len(f.requests()) }

// client is a client of the fake with every product, and the given config changes.
func (f *fake) client(change ...func(*mdrn.Config)) *mdrn.Client {
	f.t.Helper()
	cfg := mdrn.Config{Origin: f.server.URL, Project: "project_one", Environment: "sandbox", Tokens: mdrn.Tokens{Database: "database-token", Storage: "storage-token", Mail: "mail-token"}}
	for _, c := range change {
		c(&cfg)
	}
	client, err := mdrn.New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return client
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func fail(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]any{"error": map[string]any{"code": code, "message": "No", "retryable": false, "requestId": "req_1"}})
}

func record(id string, version uint64, data map[string]any) map[string]any {
	return map[string]any{"id": id, "collection": "tasks", "version": version, "data": data, "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"}
}

func mdrnError(t *testing.T, err error, status int, code string) *mdrn.Error {
	t.Helper()
	var e *mdrn.Error
	if !errors.As(err, &e) || e.Status != status || e.Code != code {
		t.Fatalf("expected %d %s, got %v", status, code, err)
	}
	return e
}

func refused(t *testing.T, err error) {
	t.Helper()
	var e *mdrn.ArgumentError
	if !errors.As(err, &e) {
		t.Fatalf("expected an ArgumentError, got %v", err)
	}
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestEveryProductRequestCarriesTheHeaders(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		switch {
		case r.Method == "POST":
			reply(w, 201, record("rec_1", 1, map[string]any{"title": "one"}))
		case r.Method == "PATCH":
			reply(w, 200, record("rec_1", 4, map[string]any{"title": "two"}))
		default:
			reply(w, 200, map[string]any{"items": []any{}, "nextCursor": nil})
		}
	})
	client := f.client()
	ctx := context.Background()
	if _, err := client.DB.Create(ctx, "tasks", map[string]any{"title": "one", "html": "<b>"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DB.Update(ctx, "tasks", "rec_1", map[string]any{"title": "two"}, mdrn.AtVersion(3), mdrn.Key("update-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DB.List(ctx, "tasks", mdrn.PageOptions{Limit: 2, Cursor: "a b/c"}); err != nil {
		t.Fatal(err)
	}
	r := f.requests()
	if len(r) != 3 {
		t.Fatalf("%d requests", len(r))
	}
	for _, s := range r {
		if s.Header.Get("Accept") != "application/json" || s.Header.Get("Authorization") != "Bearer database-token" || s.Header.Get("MDRN-Project") != "project_one" || s.Header.Get("MDRN-Environment") != "sandbox" || s.Header.Get("User-Agent") != "mdrn-sdk-go/"+mdrn.Version {
			t.Fatalf("headers of %s %s: %v", s.Method, s.Path, s.Header)
		}
	}
	if r[0].Path != "/_mdrn/v1/database/collections/tasks/records" || r[0].Header.Get("Content-Type") != "application/json" || !uuid.MatchString(r[0].Header.Get("Idempotency-Key")) || r[0].Body != `{"data":{"html":"<b>","title":"one"}}` {
		t.Fatalf("create: %+v", r[0])
	}
	if r[1].Method != "PATCH" || r[1].Header.Get("If-Match") != "3" || r[1].Header.Get("Idempotency-Key") != "update-1" || r[1].Body != `{"data":{"title":"two"}}` {
		t.Fatalf("update: %+v", r[1])
	}
	if r[2].Path != "/_mdrn/v1/database/collections/tasks/records?limit=2&cursor=a+b%2Fc" || r[2].Header.Get("Content-Type") != "" || r[2].Header.Get("Idempotency-Key") != "" || r[2].Body != "" {
		t.Fatalf("list: %+v", r[2])
	}
}

func TestArgumentErrorsSendNothing(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) { fail(w, 500, "UNEXPECTED") })
	client := f.client()
	ctx := context.Background()
	_, err := client.DB.Update(ctx, "tasks", "rec_1", map[string]any{}, mdrn.AtVersion(0))
	refused(t, err)
	refused(t, client.DB.Delete(ctx, "tasks", "rec_1", mdrn.AtVersion(0)))
	_, err = client.DB.Create(ctx, "", map[string]any{})
	refused(t, err)
	_, err = client.DB.Create(ctx, "tasks", make(chan int))
	refused(t, err)
	_, err = client.Files.Upload(ctx, mdrn.Upload{Name: "", Bytes: []byte("x")})
	refused(t, err)
	_, err = client.Files.Upload(ctx, mdrn.Upload{Name: "x.txt"})
	refused(t, err)
	_, err = client.Files.Uploads.Part(ctx, "upl_1", 0, 1)
	refused(t, err)
	_, err = client.Mail.Send(ctx, mdrn.Message{To: "a@example.com", Template: "welcome", Subject: "Hi"})
	refused(t, err)
	_, err = client.Mail.Send(ctx, mdrn.Message{To: "a@example.com", Template: "welcome", HTML: "<p>Hi</p>"})
	refused(t, err)
	_, err = client.Mail.Send(ctx, mdrn.Message{Subject: "Hi", Text: "Hello"})
	refused(t, err)
	_, err = client.Auth.Me(ctx, http.Header{"X-Mdrn-User-Id": {"user_1"}, "X-Mdrn-Identity-Token": {"t"}}, "/relative")
	refused(t, err)
	if f.count() != 0 {
		t.Fatalf("%d requests were sent", f.count())
	}
}

func TestACancelledContextSendsNothing(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) { reply(w, 200, record("rec_1", 1, nil)) })
	client := f.client()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.DB.Get(ctx, "tasks", "rec_1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	var e *mdrn.Error
	if errors.As(err, &e) {
		t.Fatal("a cancellation is not an mdrn.Error")
	}
	if f.count() != 0 {
		t.Fatal("a request was sent")
	}
}

func TestTheDeadlineIsATimeout(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		reply(w, 200, record("rec_1", 1, nil))
	})
	client := f.client(func(c *mdrn.Config) { c.Timeout = 50 * time.Millisecond })
	_, err := client.DB.Get(context.Background(), "tasks", "rec_1")
	e := mdrnError(t, err, 0, mdrn.CodeTimeout)
	if errors.Unwrap(e) == nil {
		t.Fatal("the transport error is not wrapped")
	}
}

func TestAnUnreachableServerIsANetworkError(t *testing.T) {
	client, err := mdrn.New(mdrn.Config{Origin: "http://127.0.0.1:1", Project: "p", Environment: "sandbox", Tokens: mdrn.Tokens{Database: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DB.Get(context.Background(), "tasks", "rec_1")
	mdrnError(t, err, 0, mdrn.CodeNetworkError)
}

func TestARedirectIsNeverFollowed(t *testing.T) {
	var f *fake
	f = newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.URL.Path == "/elsewhere" {
			reply(w, 200, record("rec_1", 1, nil))
			return
		}
		w.Header().Set("Location", f.server.URL+"/elsewhere")
		w.WriteHeader(307)
	})
	client := f.client()
	_, err := client.DB.Get(context.Background(), "tasks", "rec_1")
	e := mdrnError(t, err, 307, mdrn.CodeUnexpectedRedirect)
	if !strings.Contains(string(e.Details), "/elsewhere") {
		t.Fatalf("details %s", e.Details)
	}
	if f.count() != 1 {
		t.Fatalf("%d requests: the redirect was followed", f.count())
	}
}

func TestErrorsWithoutAnEnvelopeStillHaveACodeAndMessage(t *testing.T) {
	for _, tc := range []struct{ body, code, message string }{
		{`{"error": "Not found"}`, "HTTP_ERROR", "MDRN request failed with HTTP 409"},
		{`"Bad gateway"`, "HTTP_ERROR", "MDRN request failed with HTTP 409"},
		{`<html>busy</html>`, "HTTP_ERROR", "MDRN request failed with HTTP 409"},
		{`{"code": "conflict", "message": "Changed"}`, "conflict", "Changed"},
		{`{"error": {"code": "gone", "message": "Deleted"}}`, "gone", "Deleted"},
	} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
			w.WriteHeader(409)
			_, _ = io.WriteString(w, tc.body)
		})
		_, err := f.client().DB.List(context.Background(), "tasks", mdrn.PageOptions{})
		e := mdrnError(t, err, 409, tc.code)
		if e.Error() != tc.message {
			t.Fatalf("%s: message %q", tc.body, e.Error())
		}
		if json.Valid([]byte(tc.body)) != (e.Details != nil) {
			t.Fatalf("%s: details %s", tc.body, e.Details)
		}
	}
}

func TestGetAnswersNilForAMissingThing(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) { fail(w, 404, "NOT_FOUND") })
	client := f.client()
	ctx := context.Background()
	if r, err := client.DB.Get(ctx, "tasks", "rec_x"); r != nil || err != nil {
		t.Fatalf("record %v %v", r, err)
	}
	if o, err := client.Files.Get(ctx, "obj_x"); o != nil || err != nil {
		t.Fatalf("file %v %v", o, err)
	}
	if u, err := client.Files.Uploads.Get(ctx, "upl_x"); u != nil || err != nil {
		t.Fatalf("upload %v %v", u, err)
	}
	// Everything else reports the 404.
	_, err := client.Files.Operation(ctx, "op_x")
	mdrnError(t, err, 404, "NOT_FOUND")
	err = client.DB.Delete(ctx, "tasks", "rec_x", mdrn.AtVersion(1))
	mdrnError(t, err, 404, "NOT_FOUND")
}

func TestAProductWithoutATokenIsNotEnabled(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) { fail(w, 500, "UNEXPECTED") })
	client := f.client(func(c *mdrn.Config) { c.Tokens = mdrn.Tokens{Database: "database-token"} })
	if got := client.Enabled(); len(got) != 1 || got[0] != "db" {
		t.Fatalf("enabled %v", got)
	}
	ctx := context.Background()
	_, err := client.Files.Get(ctx, "obj_x")
	e := mdrnError(t, err, 0, mdrn.CodeProductNotEnabled)
	if !strings.Contains(e.Message, "MDRN_STORAGE_TOKEN") {
		t.Fatalf("message %q", e.Message)
	}
	_, err = client.Files.Upload(ctx, mdrn.Upload{Name: "x", Bytes: []byte("x")})
	mdrnError(t, err, 0, mdrn.CodeProductNotEnabled)
	_, err = client.Mail.Send(ctx, mdrn.Message{To: "a@example.com", Subject: "Hi", Text: "Hello"})
	mdrnError(t, err, 0, mdrn.CodeProductNotEnabled)
	// A token without an origin or URL is not enabled either, and says which URL is missing.
	noURL, err := mdrn.New(mdrn.Config{Project: "p", Environment: "sandbox", Tokens: mdrn.Tokens{Mail: "mail-token"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = noURL.Mail.Send(ctx, mdrn.Message{To: "a@example.com", Subject: "Hi", Text: "Hello"})
	if e := mdrnError(t, err, 0, mdrn.CodeProductNotEnabled); !strings.Contains(e.Message, "MDRN_MAIL_URL") {
		t.Fatalf("message %q", e.Message)
	}
	if f.count() != 0 {
		t.Fatal("a request was sent")
	}
}

func TestTheTokenTravelsOnlyOverHTTPS(t *testing.T) {
	base := mdrn.Config{Project: "p", Environment: "sandbox", Tokens: mdrn.Tokens{Database: "t"}}
	for _, origin := range []string{"http://app.example", "ftp://app.example", "not a url", "http://127.0.0.2"} {
		cfg := base
		cfg.Origin = origin
		_, err := mdrn.New(cfg)
		refused(t, err)
		cfg = base
		cfg.URLs.Database = origin
		if _, err := mdrn.New(cfg); err == nil {
			t.Fatalf("URL %s accepted", origin)
		}
	}
	for _, origin := range []string{"https://app.example", "https://app.example/", "http://localhost:8787", "http://127.0.0.1:8787", "http://[::1]:8787"} {
		cfg := base
		cfg.Origin = origin
		if _, err := mdrn.New(cfg); err != nil {
			t.Fatalf("%s refused: %v", origin, err)
		}
	}
	for _, cfg := range []mdrn.Config{{Environment: "sandbox"}, {Project: "p", Environment: "staging"}, {Project: "p", Environment: "sandbox", Timeout: -1}} {
		_, err := mdrn.New(cfg)
		refused(t, err)
	}
}

func TestFromEnv(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.Header.Get("Authorization") != "Bearer mail-token" || r.URL.Path != "/custom/mail/messages" {
			fail(w, 401, "PRODUCT_ACCESS_DENIED")
			return
		}
		reply(w, 202, map[string]any{"id": "msg_1", "state": "queued", "requestId": "req_1"})
	})
	for name, value := range map[string]string{
		"MDRN_PROJECT_ID": "project_one", "MDRN_ENVIRONMENT": "production",
		"MDRN_MAIL_URL": f.server.URL + "/custom/mail", "MDRN_MAIL_TOKEN": "mail-token",
		"MDRN_DATABASE_URL": "", "MDRN_DATABASE_TOKEN": "database-token", "MDRN_STORAGE_URL": "", "MDRN_STORAGE_TOKEN": "",
	} {
		t.Setenv(name, value)
	}
	client, err := mdrn.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if client.Project != "project_one" || client.Environment != "production" {
		t.Fatalf("%s %s", client.Project, client.Environment)
	}
	if got := client.Enabled(); len(got) != 1 || got[0] != "mail" {
		t.Fatalf("enabled %v (a token without a URL enables nothing)", got)
	}
	sent, err := client.Mail.Send(context.Background(), mdrn.Message{To: "a@example.com", Subject: "Hi", Text: "Hello"})
	if err != nil || sent.State != "queued" {
		t.Fatal(sent, err)
	}
	if h := f.requests()[0].Header; h.Get("MDRN-Environment") != "production" {
		t.Fatalf("environment %q", h.Get("MDRN-Environment"))
	}
}

func TestAnUpdateWithoutAVersionReadsAndRetriesAConflict(t *testing.T) {
	conflicts := 2
	version := uint64(1)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		switch r.Method {
		case "GET":
			reply(w, 200, record("rec_1", version, map[string]any{"n": 1}))
		case "PATCH":
			if conflicts > 0 {
				conflicts--
				version++
				fail(w, 409, "CONFLICT")
				return
			}
			reply(w, 200, record("rec_1", version+1, map[string]any{"n": 2}))
		}
	})
	client := f.client()
	updated, err := client.DB.Update(context.Background(), "tasks", "rec_1", map[string]any{"n": 2}, mdrn.Key("mine"))
	if err != nil || updated.Version != 4 {
		t.Fatal(updated, err)
	}
	r := f.requests()
	if len(r) != 6 {
		t.Fatalf("%d requests", len(r))
	}
	keys := map[string]bool{}
	for i, s := range r {
		want := "GET"
		if i%2 == 1 {
			want = "PATCH"
		}
		if s.Method != want {
			t.Fatalf("request %d is %s", i+1, s.Method)
		}
		if want == "PATCH" {
			if s.Header.Get("If-Match") != fmt.Sprint(i/2+1) {
				t.Fatalf("request %d If-Match %s", i+1, s.Header.Get("If-Match"))
			}
			keys[s.Header.Get("Idempotency-Key")] = true
		}
	}
	// The caller's key on the first attempt, a fresh one on every retry.
	if r[1].Header.Get("Idempotency-Key") != "mine" || len(keys) != 3 || !uuid.MatchString(r[3].Header.Get("Idempotency-Key")) {
		t.Fatalf("keys %v", keys)
	}
}

func TestAContendedWriteGivesUpAfterFourAttempts(t *testing.T) {
	version := uint64(0)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.Method == "GET" {
			version++
			reply(w, 200, record("rec_1", version, nil))
			return
		}
		fail(w, 409, "CONFLICT")
	})
	client := f.client()
	err := client.DB.Delete(context.Background(), "tasks", "rec_1")
	mdrnError(t, err, 409, "CONFLICT")
	if f.count() != 8 {
		t.Fatalf("%d requests", f.count())
	}
	// With a version: one request, never retried.
	err = client.DB.Delete(context.Background(), "tasks", "rec_1", mdrn.AtVersion(7))
	mdrnError(t, err, 409, "CONFLICT")
	if r := f.requests(); len(r) != 9 || r[8].Method != "DELETE" || r[8].Header.Get("If-Match") != "7" {
		t.Fatalf("%d requests, last %+v", len(r), r[len(r)-1])
	}
}

// uploadFake is an upload session with parts of partBytes: the session is pending on its
// first read, the object verifying on its first read, and the final object state is given.
func uploadFake(t *testing.T, partBytes int, finalState string) *fake {
	reads := map[string]int{}
	var f *fake
	f = newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.URL.Path == "/transfer/g1" {
			if r.Header.Get("Authorization") != "" || r.Header.Get("MDRN-Project") != "" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Mdrn-Grant") != "g1" {
				t.Errorf("a grant request carried the wrong headers: %v", r.Header)
			}
			reply(w, 200, map[string]any{"partNumber": 1, "bytes": len(body), "etag": fmt.Sprintf("\"etag-%d\"", len(body))})
			return
		}
		reads[r.Method+" "+r.URL.Path]++
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/storage/uploads":
			reply(w, 202, map[string]any{"id": "upl_1", "objectId": "obj_1", "state": "pending", "expiresAt": "2026-01-02T00:00:00Z", "operationId": nil, "partBytes": partBytes})
		case "GET /v1/storage/uploads/upl_1":
			state := "uploading"
			if reads["GET /v1/storage/uploads/upl_1"] == 1 {
				state = "pending"
			}
			reply(w, 200, map[string]any{"id": "upl_1", "objectId": "obj_1", "state": state, "expiresAt": "2026-01-02T00:00:00Z", "operationId": nil, "partBytes": partBytes})
		case "POST /v1/storage/uploads/upl_1/parts":
			reply(w, 202, map[string]any{"id": "g1", "url": f.server.URL + "/transfer/g1", "method": "PUT", "headers": map[string]string{"x-mdrn-grant": "g1"}, "expiresAt": "2026-01-01T00:15:00Z", "maxBytes": partBytes})
		case "POST /v1/storage/uploads/upl_1/complete":
			reply(w, 202, map[string]any{"id": "obj_1", "filename": "f.bin", "contentType": "application/octet-stream", "bytes": 10, "state": "verifying", "operationId": "op_1"})
		case "GET /v1/storage/objects/obj_1":
			state := finalState
			if reads["GET /v1/storage/objects/obj_1"] == 1 {
				state = "verifying"
			}
			reply(w, 200, map[string]any{"id": "obj_1", "filename": "f.bin", "contentType": "application/octet-stream", "bytes": 10, "sha256": "x", "state": state, "createdAt": "2026-01-01T00:00:00Z", "deletedAt": nil, "purgeAt": nil})
		default:
			fail(w, 404, "NOT_FOUND")
		}
	})
	return f
}

func TestUploadTakesThePartSizeFromTheSessionAndPolls(t *testing.T) {
	f := uploadFake(t, 4, "ready")
	client := f.client()
	file, err := client.Files.Upload(context.Background(), mdrn.Upload{Name: "f.bin", Bytes: []byte("0123456789")}, mdrn.Key("k"))
	if err != nil || file.State != "ready" {
		t.Fatal(file, err)
	}
	var steps []string
	for _, s := range f.requests() {
		steps = append(steps, s.Method+" "+s.Path+" "+s.Header.Get("Idempotency-Key"))
	}
	want := []string{
		"POST /v1/storage/uploads k",
		"GET /v1/storage/uploads/upl_1 ", // pending
		"GET /v1/storage/uploads/upl_1 ", // uploading
		"POST /v1/storage/uploads/upl_1/parts k-part-1", "PUT /transfer/g1 ",
		"POST /v1/storage/uploads/upl_1/parts k-part-2", "PUT /transfer/g1 ",
		"POST /v1/storage/uploads/upl_1/parts k-part-3", "PUT /transfer/g1 ",
		"POST /v1/storage/uploads/upl_1/complete k-complete",
		"GET /v1/storage/objects/obj_1 ", // verifying
		"GET /v1/storage/objects/obj_1 ", // ready
	}
	if strings.Join(steps, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps:\n%s", strings.Join(steps, "\n"))
	}
	r := f.requests()
	if r[0].Body != `{"filename":"f.bin","contentType":"application/octet-stream","bytes":10,"sha256":"84d89877f0d4041efb6bf91a16f0248f2fd573e6af05c19f96bedb9f882f7882"}` {
		t.Fatalf("create %s", r[0].Body)
	}
	if r[3].Body != `{"partNumber":1,"bytes":4}` || r[7].Body != `{"partNumber":3,"bytes":2}` || r[4].Body != "0123" || r[8].Body != "89" {
		t.Fatalf("parts %s %s %s %s", r[3].Body, r[7].Body, r[4].Body, r[8].Body)
	}
	if r[9].Body != `{"parts":[{"partNumber":1,"etag":"\"etag-4\""},{"partNumber":2,"etag":"\"etag-4\""},{"partNumber":3,"etag":"\"etag-2\""}]}` {
		t.Fatalf("complete %s", r[9].Body)
	}
}

func TestUploadNoWaitReadsOnceAndARejectionIsAnError(t *testing.T) {
	f := uploadFake(t, 16, "rejected")
	client := f.client()
	file, err := client.Files.Upload(context.Background(), mdrn.Upload{Name: "f.bin", Bytes: []byte("0123456789")}, mdrn.NoWait())
	if err != nil || file.State != "verifying" {
		t.Fatal(file, err)
	}
	if n := f.count(); n != 7 {
		t.Fatalf("%d requests (one GET of the object expected)", n)
	}
	_, err = client.Files.Upload(context.Background(), mdrn.Upload{Name: "f.bin", Bytes: []byte("0123456789")})
	mdrnError(t, err, 0, mdrn.CodeFileRejected)
}

func TestMailSendsOnlyTheFieldsGiven(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		reply(w, 202, map[string]any{"id": "msg_1", "state": "queued", "requestId": "req_1"})
	})
	client := f.client()
	ctx := context.Background()
	for _, tc := range []struct {
		message mdrn.Message
		body    string
	}{
		{mdrn.Message{To: "a@example.com", Subject: "Hi", Text: "Hello"}, `{"recipient":"a@example.com","subject":"Hi","text":"Hello"}`},
		{mdrn.Message{To: "a@example.com", Subject: "Hi", Text: "Hello", HTML: "<p>Hello & bye</p>"}, `{"recipient":"a@example.com","subject":"Hi","text":"Hello","html":"<p>Hello & bye</p>"}`},
		{mdrn.Message{To: "a@example.com", Template: "welcome", SenderID: "snd_1", Attachments: []mdrn.Attachment{{Filename: "a.txt", Type: "text/plain", Content: "aGk="}}}, `{"recipient":"a@example.com","templateId":"welcome","attachments":[{"filename":"a.txt","type":"text/plain","content":"aGk="}],"senderId":"snd_1"}`},
	} {
		if _, err := client.Mail.Send(ctx, tc.message); err != nil {
			t.Fatal(err)
		}
		r := f.requests()
		if last := r[len(r)-1]; last.Body != tc.body || last.Path != "/v1/mail/messages" || last.Header.Get("Authorization") != "Bearer mail-token" {
			t.Fatalf("got %s at %s", last.Body, last.Path)
		}
	}
}

func TestAuthMeAsksIdentityAtTheRequestsOrigin(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		switch r.Header.Get("Authorization") {
		case "Bearer identity-token-1":
			reply(w, 200, map[string]any{"id": "user_1", "email": "user_1@example.com", "metadataState": "not_configured"})
		case "Bearer expired":
			fail(w, 401, "UNAUTHENTICATED")
		default:
			fail(w, 503, "UNAVAILABLE")
		}
	})
	client := f.client()
	ctx := context.Background()
	h := http.Header{}
	h.Set("x-mdrn-user-id", "user_1")
	h.Set("x-mdrn-identity-token", "identity-token-1")
	me, err := client.Auth.Me(ctx, h, f.server.URL+"/account?x=1")
	if err != nil || me == nil || me.ID != "user_1" || me.Email != "user_1@example.com" || me.MetadataState != "not_configured" {
		t.Fatal(me, err)
	}
	r := f.requests()[0]
	if r.Path != "/v1/auth/me" || r.Header.Get("MDRN-Project") != "project_one" || r.Header.Get("MDRN-Environment") != "sandbox" || r.Header.Get("Accept") != "application/json" {
		t.Fatalf("%+v", r)
	}
	h.Set("x-mdrn-identity-token", "expired")
	if me, err := client.Auth.Me(ctx, h, f.server.URL+"/account"); me != nil || err != nil {
		t.Fatal(me, err)
	}
	h.Set("x-mdrn-identity-token", "broken")
	_, err = client.Auth.Me(ctx, h, f.server.URL+"/account")
	mdrnError(t, err, 503, "UNAVAILABLE")
	if me, err := client.Auth.Me(ctx, http.Header{}, f.server.URL+"/account"); me != nil || err != nil || f.count() != 3 {
		t.Fatal("without a visitor, nothing is asked")
	}
}

func TestUserFromAndScheduledRunFrom(t *testing.T) {
	h := http.Header{}
	if mdrn.UserFrom(h) != nil || mdrn.ScheduledRunFrom(h) != nil {
		t.Fatal("nothing from no headers")
	}
	h.Set("x-mdrn-user-id", "not valid!")
	h.Set("x-mdrn-identity-token", "t")
	if mdrn.UserFrom(h) != nil {
		t.Fatal("a malformed id is nobody")
	}
	h.Set("x-mdrn-user-id", "user_1")
	if u := mdrn.UserFrom(h); u == nil || u.ID != "user_1" || u.Token != "t" {
		t.Fatalf("%+v", u)
	}
	h.Set("x-mdrn-schedule", "digest")
	if mdrn.ScheduledRunFrom(h) != nil {
		t.Fatal("a schedule needs a run id")
	}
	h.Set("x-mdrn-run-id", "run_1")
	h.Set("x-mdrn-scheduled-time", "2026-09-26T03:00:00Z")
	h.Set("x-mdrn-attempt", "2")
	run := mdrn.ScheduledRunFrom(h)
	if run == nil || run.Schedule != "digest" || run.RunID != "run_1" || run.Attempt != 2 || !run.ScheduledTime.Equal(time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", run)
	}
	h.Del("x-mdrn-attempt")
	if run := mdrn.ScheduledRunFrom(h); run.Attempt != 1 {
		t.Fatalf("attempt %d without the header", run.Attempt)
	}
}

func TestRecordDecode(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		reply(w, 200, record("rec_1", 2, map[string]any{"title": "one", "done": true}))
	})
	rec, err := f.client().DB.Get(context.Background(), "tasks", "rec_1")
	if err != nil {
		t.Fatal(err)
	}
	var task struct {
		Title string `json:"title"`
		Done  bool   `json:"done"`
	}
	if err := rec.Decode(&task); err != nil || task.Title != "one" || !task.Done || rec.Version != 2 || rec.CreatedAt.IsZero() {
		t.Fatalf("%+v %+v %v", rec, task, err)
	}
}
