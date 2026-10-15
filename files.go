package mdrn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Files is the Files product: uploads, the files they become, download URLs. Uploads is the
// upload session as the wire has it, for resuming an upload or for letting a browser send the
// bytes.
type Files struct {
	c       *Client
	Uploads *Uploads
}

// Uploads is the upload session: create, part grants, completion, abort, and reading it back.
type Uploads struct {
	c *Client
}

// Upload is a file to upload with Files.Upload: its name, its content type
// ("application/octet-stream" when empty) and its bytes.
type Upload struct {
	Name  string
	Type  string
	Bytes []byte
}

// File is a stored file. State is "verifying" until MDRN has checked the bytes, then "ready"
// or "rejected"; a deleted file keeps DeletedAt and PurgeAt.
type File struct {
	ID          string     `json:"id"`
	Filename    string     `json:"filename"`
	ContentType string     `json:"contentType"`
	Bytes       int64      `json:"bytes"`
	SHA256      string     `json:"sha256"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"createdAt"`
	DeletedAt   *time.Time `json:"deletedAt"`
	PurgeAt     *time.Time `json:"purgeAt"`
}

// Download is a plain URL the file can be fetched from until ExpiresAt (15 minutes), with no
// credentials: the URL is the whole credential.
type Download struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// UploadRequest opens an upload session: the file's name, content type, size and SHA-256
// (lowercase hex).
type UploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
}

// UploadSession is an upload in progress. State goes pending, uploading, verifying, then
// complete, rejected, aborted or expired. PartBytes is the size of every part but the last.
type UploadSession struct {
	ID          string    `json:"id"`
	ObjectID    string    `json:"objectId"`
	State       string    `json:"state"`
	ExpiresAt   time.Time `json:"expiresAt"`
	OperationID string    `json:"operationId"`
	PartBytes   int64     `json:"partBytes"`
}

// Grant is a one-time URL of the Files gateway: send the bytes with exactly Method, URL and
// Headers, through a plain HTTP client, with no product token and no MDRN headers.
type Grant struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expiresAt"`
	MaxBytes  int64             `json:"maxBytes"`
}

// Part is one part of an upload as the session lists it.
type Part struct {
	PartNumber int    `json:"partNumber"`
	Bytes      int64  `json:"bytes"`
	ETag       string `json:"etag"`
	State      string `json:"state"`
}

// CompletedPart names a part sent to its grant, with the etag the gateway answered.
type CompletedPart struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
}

// Completion is the answer to completing an upload: the file (ID) in state "verifying", and
// the operation that verifies it.
type Completion struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Bytes       int64  `json:"bytes"`
	State       string `json:"state"`
	OperationID string `json:"operationId"`
}

// Operation is a background operation of Files, such as the verification of an upload.
type Operation struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	State          string     `json:"state"`
	ProcessedBytes int64      `json:"processedBytes"`
	ProcessedItems int64      `json:"processedItems"`
	Attempts       int64      `json:"attempts"`
	RetryAt        *time.Time `json:"retryAt"`
	Error          string     `json:"error"`
}

func checkID(what, id string) error {
	if id == "" {
		return argument("%s is required", what)
	}
	return nil
}

// Upload does the whole upload: the file's SHA-256, an upload session, one part after another
// (of the size the session says, 16 MiB) sent to a grant URL with the grant's own headers
// through Transfer, completion, and then waits for MDRN to verify the bytes (polled every
// second, a minute at most) and answers the file in state "ready". Options: Key (the steps
// use <key>, <key>-part-1, <key>-part-2, ..., <key>-complete), NoWait (one read of the object
// after completion, usually still "verifying"). Bytes that do not match are FILE_REJECTED.
func (f *Files) Upload(ctx context.Context, file Upload, opts ...Option) (*File, error) {
	o, err := options(opts)
	if err != nil {
		return nil, err
	}
	if file.Name == "" {
		return nil, argument("name is required")
	}
	if len(file.Bytes) == 0 {
		return nil, argument("bytes are required")
	}
	if f.c.files.url == "" {
		return nil, &Error{Code: CodeProductNotEnabled, Message: f.c.files.unavailable}
	}
	key := o.keyOrNew()
	contentType := file.Type
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	digest := sha256.Sum256(file.Bytes)
	opened, err := f.Uploads.Create(ctx, UploadRequest{Filename: file.Name, ContentType: contentType, Bytes: int64(len(file.Bytes)), SHA256: hex.EncodeToString(digest[:])}, Key(key))
	if err != nil {
		return nil, err
	}
	// The create response's state is never trusted: the session is read until it is not
	// pending, and must then be uploading.
	var session *UploadSession
	err = f.c.until(ctx, func() (bool, error) {
		session, err = f.Uploads.Get(ctx, opened.ID)
		return err != nil || session == nil || session.State != "pending", err
	})
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, &Error{Code: CodeUploadFailed, Message: "The upload is gone, not uploading"}
	}
	if session.State != "uploading" {
		details, _ := json.Marshal(session)
		return nil, &Error{Code: CodeUploadFailed, Message: "The upload is " + session.State + ", not uploading", Details: details}
	}
	partBytes := session.PartBytes
	if partBytes <= 0 {
		partBytes = defaultPartBytes
	}
	var parts []CompletedPart
	for offset, partNumber := int64(0), 1; offset < int64(len(file.Bytes)); offset, partNumber = offset+partBytes, partNumber+1 {
		end := offset + partBytes
		if end > int64(len(file.Bytes)) {
			end = int64(len(file.Bytes))
		}
		chunk := file.Bytes[offset:end]
		grant, err := f.Uploads.Part(ctx, session.ID, partNumber, int64(len(chunk)), Key(fmt.Sprintf("%s-part-%d", key, partNumber)))
		if err != nil {
			return nil, err
		}
		sent, err := f.c.transferBytes(ctx, grant, chunk)
		if err != nil {
			return nil, err
		}
		var result struct {
			ETag string `json:"etag"`
		}
		_ = json.Unmarshal(sent.body, &result)
		if sent.status < 200 || sent.status >= 300 {
			var details json.RawMessage
			if json.Valid(sent.body) {
				details = sent.body
			}
			return nil, &Error{Status: sent.status, Code: CodeUploadFailed, Message: fmt.Sprintf("Part %d was not accepted (HTTP %d)", partNumber, sent.status), Details: details}
		}
		parts = append(parts, CompletedPart{PartNumber: partNumber, ETag: result.ETag})
	}
	completed, err := f.Uploads.Complete(ctx, session.ID, parts, Key(key+"-complete"))
	if err != nil {
		return nil, err
	}
	var object *File
	if o.noWait {
		object, err = f.Get(ctx, completed.ID)
	} else {
		err = f.c.until(ctx, func() (bool, error) {
			object, err = f.Get(ctx, completed.ID)
			return err != nil || object == nil || object.State != "verifying", err
		})
	}
	if err != nil {
		return nil, err
	}
	if object == nil {
		return nil, &Error{Code: CodeFileRejected, Message: "The file is gone"}
	}
	if object.State == "rejected" {
		details, _ := json.Marshal(object)
		return nil, &Error{Code: CodeFileRejected, Message: "Files rejected the upload: its bytes did not match the declared size or SHA-256", Details: details}
	}
	return object, nil
}

// transferBytes sends body to a grant's URL with exactly the grant's method and headers,
// through the Transfer client: no token, no MDRN headers.
func (c *Client) transferBytes(ctx context.Context, grant *Grant, body []byte) (*response, error) {
	return c.exchange(ctx, c.transfer, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, grant.Method, grant.URL, bytes.NewReader(body))
		if err != nil {
			return nil, &Error{Code: CodeUploadFailed, Message: "The grant cannot be used: " + err.Error(), cause: err}
		}
		for name, value := range grant.Headers {
			req.Header.Set(name, value)
		}
		return req, nil
	})
}

// until polls once a second, for at most a minute, until done holds.
func (c *Client) until(ctx context.Context, read func() (done bool, err error)) error {
	deadline := time.Now().Add(waitLimit)
	for {
		done, err := read()
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return &Error{Code: CodeTimeout, Message: fmt.Sprintf("Gave up waiting after %s", waitLimit)}
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

// Get is the file, or nil (and no error) when there is none.
func (f *Files) Get(ctx context.Context, id string) (*File, error) {
	if err := checkID("id", id); err != nil {
		return nil, err
	}
	var file File
	found, err := f.c.read(ctx, f.c.files, "/objects/"+segment(id), &file)
	if err != nil || !found {
		return nil, err
	}
	return &file, nil
}

// List is one page of the files in state ready.
func (f *Files) List(ctx context.Context, page PageOptions) (*Page[File], error) {
	var result Page[File]
	if err := f.c.call(ctx, f.c.files, http.MethodGet, "/objects"+page.query(), nil, "", 0, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DownloadURL is a plain URL the file can be fetched from for 15 minutes. Options: Key.
func (f *Files) DownloadURL(ctx context.Context, id string, opts ...Option) (*Download, error) {
	o, err := options(opts)
	if err != nil {
		return nil, err
	}
	if err := checkID("id", id); err != nil {
		return nil, err
	}
	var grant Grant
	if err := f.c.call(ctx, f.c.files, http.MethodPost, "/objects/"+segment(id)+"/download-grants", map[string]any{}, o.keyOrNew(), 0, &grant); err != nil {
		return nil, err
	}
	return &Download{URL: grant.URL, ExpiresAt: grant.ExpiresAt}, nil
}

// Delete removes the file. Options: Key.
func (f *Files) Delete(ctx context.Context, id string, opts ...Option) error {
	o, err := options(opts)
	if err != nil {
		return err
	}
	if err := checkID("id", id); err != nil {
		return err
	}
	return f.c.call(ctx, f.c.files, http.MethodDelete, "/objects/"+segment(id), nil, o.keyOrNew(), 0, nil)
}

// Operation is a background operation of Files, such as the one a Completion names.
func (f *Files) Operation(ctx context.Context, id string) (*Operation, error) {
	if err := checkID("id", id); err != nil {
		return nil, err
	}
	var operation Operation
	if err := f.c.call(ctx, f.c.files, http.MethodGet, "/operations/"+segment(id), nil, "", 0, &operation); err != nil {
		return nil, err
	}
	return &operation, nil
}

// Create opens an upload session. Options: Key.
func (u *Uploads) Create(ctx context.Context, input UploadRequest, opts ...Option) (*UploadSession, error) {
	o, err := options(opts)
	if err != nil {
		return nil, err
	}
	var session UploadSession
	if err := u.c.call(ctx, u.c.files, http.MethodPost, "/uploads", input, o.keyOrNew(), 0, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// Get is the session, or nil (and no error) when there is none.
func (u *Uploads) Get(ctx context.Context, uploadID string) (*UploadSession, error) {
	if err := checkID("uploadID", uploadID); err != nil {
		return nil, err
	}
	var session UploadSession
	found, err := u.c.read(ctx, u.c.files, "/uploads/"+segment(uploadID), &session)
	if err != nil || !found {
		return nil, err
	}
	return &session, nil
}

// List is one page of the upload sessions.
func (u *Uploads) List(ctx context.Context, page PageOptions) (*Page[UploadSession], error) {
	var result Page[UploadSession]
	if err := u.c.call(ctx, u.c.files, http.MethodGet, "/uploads"+page.query(), nil, "", 0, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Parts is one page of the session's parts.
func (u *Uploads) Parts(ctx context.Context, uploadID string, page PageOptions) (*Page[Part], error) {
	if err := checkID("uploadID", uploadID); err != nil {
		return nil, err
	}
	var result Page[Part]
	if err := u.c.call(ctx, u.c.files, http.MethodGet, "/uploads/"+segment(uploadID)+"/parts"+page.query(), nil, "", 0, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Part asks for the grant of one part: partNumber from 1, size its bytes (at most the
// session's PartBytes). Options: Key.
func (u *Uploads) Part(ctx context.Context, uploadID string, partNumber int, size int64, opts ...Option) (*Grant, error) {
	o, err := options(opts)
	if err != nil {
		return nil, err
	}
	if err := checkID("uploadID", uploadID); err != nil {
		return nil, err
	}
	if partNumber < 1 {
		return nil, argument("partNumber must be a positive integer")
	}
	if size < 1 {
		return nil, argument("size must be a positive integer")
	}
	body := struct {
		PartNumber int   `json:"partNumber"`
		Bytes      int64 `json:"bytes"`
	}{partNumber, size}
	var grant Grant
	if err := u.c.call(ctx, u.c.files, http.MethodPost, "/uploads/"+segment(uploadID)+"/parts", body, o.keyOrNew(), 0, &grant); err != nil {
		return nil, err
	}
	return &grant, nil
}

// Complete closes the session with the parts sent, in order, and starts verification.
// Options: Key.
func (u *Uploads) Complete(ctx context.Context, uploadID string, parts []CompletedPart, opts ...Option) (*Completion, error) {
	o, err := options(opts)
	if err != nil {
		return nil, err
	}
	if err := checkID("uploadID", uploadID); err != nil {
		return nil, err
	}
	if parts == nil {
		parts = []CompletedPart{}
	}
	body := struct {
		Parts []CompletedPart `json:"parts"`
	}{parts}
	var completion Completion
	if err := u.c.call(ctx, u.c.files, http.MethodPost, "/uploads/"+segment(uploadID)+"/complete", body, o.keyOrNew(), 0, &completion); err != nil {
		return nil, err
	}
	return &completion, nil
}

// Abort ends a session that is pending or uploading. Options: Key.
func (u *Uploads) Abort(ctx context.Context, uploadID string, opts ...Option) error {
	o, err := options(opts)
	if err != nil {
		return err
	}
	if err := checkID("uploadID", uploadID); err != nil {
		return err
	}
	return u.c.call(ctx, u.c.files, http.MethodDelete, "/uploads/"+segment(uploadID), nil, o.keyOrNew(), 0, nil)
}
