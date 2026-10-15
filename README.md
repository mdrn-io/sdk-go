# MDRN SDK for Go

`github.com/mdrn-io/sdk-go` (package `mdrn`) is the client for the products a deployed MDRN app
has (Database, Files, Mail) and for who is calling it (Auth), for Go 1.23+. It needs only the
standard library. The same surface exists for [JavaScript, Python and Rust](../README.md).

## Install

```sh
go get github.com/mdrn-io/sdk-go
```

## Quickstart

```go
import mdrn "github.com/mdrn-io/sdk-go"

client, err := mdrn.FromEnv() // in main, once; err names the MDRN_* variable that is wrong
http.HandleFunc("/notes", func(w http.ResponseWriter, r *http.Request) {
	user := mdrn.UserFrom(r.Header)
	if user == nil {
		http.Error(w, "Sign in first", http.StatusUnauthorized)
		return
	}
	note, err := client.DB.Create(r.Context(), "notes", map[string]any{"owner": user.ID, "text": "hello"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(note)
})
```

## Where the values come from

MDRN injects everything `FromEnv` reads into a release's environment: `MDRN_PROJECT_ID`,
`MDRN_ENVIRONMENT` (`sandbox` or `production`), and one `MDRN_<PRODUCT>_URL` and
`MDRN_<PRODUCT>_TOKEN` per product switched on for the app (`DATABASE`, `STORAGE` for Files,
`MAIL`). Anywhere else, give the values yourself:

```go
client, err := mdrn.New(mdrn.Config{
	Origin:      "https://your-app.apps.mdrn.io",
	Project:     project,
	Environment: "sandbox",
	Tokens:      mdrn.Tokens{Database: databaseToken, Storage: storageToken, Mail: mailToken},
})
```

The tokens are workload credentials, never for a browser. `client.Enabled()` lists the
products the client has (`db`, `files`, `mail`); a call on another returns `PRODUCT_NOT_ENABLED`
with the variable that is missing. `Config` also takes `URLs` (product URLs that replace the
ones derived from `Origin`), `HTTP` (a `Doer`, anything with `Do(*http.Request)`, that sends
every product request; the default `http.Client` never follows a redirect, and yours must not
either), `Transfer` (the plain client that sends bytes to grant URLs; never one that adds a
token) and `Timeout` (10 s; applied through each request's context, so it holds for any
`Doer`). In a Worker, product requests go through the `MDRN_PRODUCTS` binding; a `Doer` backed
by it is where a TinyGo build of the SDK would plug in, which does not exist yet.

Every call takes a `context.Context` first; a context already cancelled sends nothing and
returns its error. Every write takes options: `mdrn.Key("...")` gives it its idempotency key
(a UUID is generated when you give none; give one when you retry a change yourself and want
it to happen once).

## Database

A record is a JSON object in a named collection, with an id MDRN assigns and a version that
grows by one on every change. `Data` is the object as `json.RawMessage`; `Decode` reads it into
a value of yours.

```go
task, err := client.DB.Create(ctx, "tasks", map[string]any{"title": "Write docs", "done": false})
same, err := client.DB.Get(ctx, "tasks", task.ID)                       // nil, nil when there is none
page, err := client.DB.List(ctx, "tasks", mdrn.PageOptions{Limit: 50, Cursor: cursor}) // Items, NextCursor
done, err := client.DB.Update(ctx, "tasks", task.ID, map[string]any{"done": true})
err = client.DB.Delete(ctx, "tasks", task.ID)
```

`Update` merges the fields you give into the record (a shallow merge). Without a version it
reads the record first, sends the change with the version it read as `If-Match`, and if the
record changed in between (409 `CONFLICT`) it reads and tries again, up to three more times.
With `mdrn.AtVersion(v)` the change is sent once with that `If-Match` and a `CONFLICT` is yours
to handle; that is the right choice when the user saw the version they are changing. `Delete`
works the same way. Keep cursors exactly as returned; `NextCursor` is `""` after the last page.

## Files

```go
file, err := client.Files.Upload(ctx, mdrn.Upload{Name: "photo.jpg", Type: "image/jpeg", Bytes: bytes})
info, err := client.Files.Get(ctx, file.ID)                          // nil, nil when there is none
page, err := client.Files.List(ctx, mdrn.PageOptions{Limit: 50})
download, err := client.Files.DownloadURL(ctx, file.ID)              // URL, ExpiresAt: a plain URL, 15 minutes
err = client.Files.Delete(ctx, file.ID)
```

`Upload` does the whole upload: the file's SHA-256, an upload session, one part of 16 MiB after
another sent to a grant URL with the grant's own headers (through `Transfer`, a plain client,
never with the token), completion, and then waits for MDRN to verify the bytes (polled every
second, a minute at most) and answers the file in state `ready`. Pass `mdrn.NoWait()` to get it
back after one read instead (usually still `verifying`) and check `Files.Get` later. Bytes that
do not match return `FILE_REJECTED`.

The session itself is there too, for resuming an upload or for letting a browser PUT the
bytes: `client.Files.Uploads.Create`, `client.Files.Uploads.Get`, `client.Files.Uploads.List`,
`client.Files.Uploads.Parts`, `client.Files.Uploads.Part`, `client.Files.Uploads.Complete`,
`client.Files.Uploads.Abort` and `client.Files.Operation(ctx, id)`. Ask for a part grant in
your backend and hand the browser the grant's `URL`, `Method` and `Headers`.

## Mail

```go
sent, err := client.Mail.Send(ctx, mdrn.Message{To: "ada@example.com", Subject: "Welcome", Text: "Hello Ada", HTML: "<p>Hello Ada</p>"})
sent, err := client.Mail.Send(ctx, mdrn.Message{To: "ada@example.com", Template: "welcome"})
```

A message is either your own content (`Subject` and `Text`, `HTML` if wanted) or a `Template`
of the project and environment, which supplies all three; giving a template together with any
of them returns an `*mdrn.ArgumentError` before anything is sent. `Attachments` (up to four,
`{Filename, Type, Content}` with base64 content) and `SenderID` (a verified sender domain; leave
it empty when the workspace has one) go with either. The answer is `{ID, State: "queued"}`:
accepted, not yet delivered. A Sandbox environment delivers like Production: to anyone, from
a verified sender.

## Auth

MDRN's Proxy deletes every `x-mdrn-*` header a client sent and, for a signed-in visitor, adds
`x-mdrn-user-id` and `x-mdrn-identity-token`. Your backend is reachable only through the Proxy,
so these can be trusted there (not on a development server anyone can reach directly).

```go
user := mdrn.UserFrom(r.Header)                      // *User{ID, Token} or nil
me, err := client.Auth.Me(ctx, r.Header, r.URL.String()) // *Profile{ID, Email, Name, MetadataState, ...} or nil
```

`Me` asks MDRN Identity (`GET /v1/auth/me` at the request's origin, with the visitor's
short-lived token) and answers nil when nobody is signed in or the session has ended. Give it
the request's absolute URL (behind `net/http`, `"https://" + r.Host + r.URL.RequestURI()` when
`r.URL` is relative).

## Schedules

A schedule declared in `mdrn.jsonc` arrives as a `POST` to its path. `ScheduledRunFrom(header)`
returns the run, or nil for any other request. `RunID` is the same on every attempt of one
run, so use it as the idempotency key of the work.

```go
if run := mdrn.ScheduledRunFrom(r.Header); run != nil && r.URL.Path == "/tasks/digest" {
	err = sendDigest(ctx, run.RunID)
}
```

## Errors

Every failed call returns `*mdrn.Error` with `Status` (the HTTP status, 0 when no response was
received), `Code`, `Message` (MDRN's, for your logs) and `Details` (the response body when it
was JSON). A call the SDK refuses before sending anything (a zero version, a template with
content, a file without a name or bytes) returns `*mdrn.ArgumentError`.

| `Code` | `Status` | When |
| --- | --- | --- |
| `PRODUCT_NOT_ENABLED` | 0 | The product has no URL or token in this environment |
| `TIMEOUT` | 0 | No answer within 10 seconds (`Config.Timeout`) |
| `NETWORK_ERROR` | 0 | The request could not be sent (`errors.Unwrap` has the cause) |
| `UNEXPECTED_REDIRECT` | 3xx | MDRN answered with a redirect, which is never followed |
| `HTTP_ERROR` | 4xx, 5xx | A failure without MDRN's error envelope |
| `INVALID_REQUEST`, `CONFLICT`, `NOT_FOUND`, `TEMPLATE_NOT_FOUND`, ... | 4xx, 5xx | The code from MDRN's envelope |
| `UPLOAD_FAILED`, `FILE_REJECTED` | 0 | An upload did not accept parts, or MDRN rejected the bytes |

`Get` methods answer `nil, nil` for a 404 instead of an error. `Code` comes from MDRN's envelope
(`{"error": {"code", "message"}}`), or is `HTTP_ERROR` when the body is not one. A redirect is
`UNEXPECTED_REDIRECT`: never followed, so the token cannot reach another origin. Nothing is
retried except the conflict loop of `Update` and `Delete`. The product URLs must be https;
plain http is accepted only to `localhost`, `127.0.0.1` or `[::1]`. The SDK's own codes are the
`mdrn.Code*` constants.

```go
var e *mdrn.Error
if errors.As(err, &e) && e.Code == "CONFLICT" {
	// reload
}
```

## Testing

The SDK has no fake of its own: the JavaScript SDK's `@mdrn/sdk/fake` is the in-memory MDRN all
four languages test against, and `sdk/conformance/server.mjs` serves it over HTTP on this
machine. With Node.js present, `go test ./...` starts it for the conformance scenarios and for
[`examples/roundtrip`](examples/roundtrip/main.go), a program that uses most of the SDK (run it
yourself with the `MDRN_*` variables set: `go run ./examples/roundtrip`). In your own tests,
start the server (`node sdk/conformance/server.mjs` prints its port) and build a client with
`Origin: "http://127.0.0.1:<port>"`, project `project_one`, environment `sandbox` and the
tokens `database-token`, `storage-token` and `mail-token`; or give `Config.HTTP` a `Doer` of
your own that answers what a test needs.

## Versions

`0.3.0` (`mdrn.Version`), shared with the other languages (`sdk/VERSION`, `sdk/CHANGELOG.md`).
All four SDKs produce the same transcript against the same scenarios: `sdk/conformance/`, where
`go run ./conformance` is the Go runner. The module is published by
`.github/workflows/publish-tools.yml`: this directory is pushed to `github.com/mdrn-io/sdk-go`
and tagged `v0.3.0`, which `proxy.golang.org` serves.
