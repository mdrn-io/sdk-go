// The Go conformance runner: every scenario of sdk/conformance/scenarios.md, against the MDRN
// at MDRN_ORIGIN, printed to stdout as the one JSON object that must equal
// sdk/conformance/expected.json. `node sdk/conformance/run.mjs` starts the fake MDRN and runs
// it as `go run ./conformance`, then compares the requests it sent with the other languages'.
// main_test.go runs the same scenarios under `go test`.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	mdrn "github.com/mdrn-io/sdk-go"
)

type settings struct {
	origin, project, environment string
	tokens                       mdrn.Tokens
	userID, identityToken        string
}

func fromEnv() settings {
	value := func(name, fallback string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return fallback
	}
	return settings{
		origin:        os.Getenv("MDRN_ORIGIN"),
		project:       value("MDRN_PROJECT", "project_one"),
		environment:   value("MDRN_ENVIRONMENT", "sandbox"),
		tokens:        mdrn.Tokens{Database: value("MDRN_DATABASE_TOKEN", "database-token"), Storage: value("MDRN_STORAGE_TOKEN", "storage-token"), Mail: value("MDRN_MAIL_TOKEN", "mail-token")},
		userID:        value("MDRN_USER_ID", "user_1"),
		identityToken: value("MDRN_IDENTITY_TOKEN", "identity-token-1"),
	}
}

func main() {
	s := fromEnv()
	if s.origin == "" {
		fmt.Fprintln(os.Stderr, "MDRN_ORIGIN is required")
		os.Exit(2)
	}
	transcript, err := conformance(context.Background(), s)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := json.MarshalIndent(transcript, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(append(out, '\n'))
}

// failure is a call's error as the transcript records it: the SDK's error with its status and
// code, or a refusal (an ArgumentError or anything else raised before a request).
func failure(err error) map[string]any {
	var e *mdrn.Error
	if errors.As(err, &e) {
		return map[string]any{"error": map[string]any{"status": e.Status, "code": e.Code}}
	}
	if os.Getenv("MDRN_CONFORMANCE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "refused: %v\n", err)
	}
	return map[string]any{"refused": true}
}

func attempt(err error) map[string]any {
	if err == nil {
		return map[string]any{"accepted": true}
	}
	return failure(err)
}

func data(r *mdrn.Record) any {
	var v any
	_ = r.Decode(&v)
	return v
}

// conformance runs the scenarios in order and answers the transcript.
func conformance(ctx context.Context, s settings) (map[string]any, error) {
	base := mdrn.Config{Project: s.project, Environment: s.environment, Tokens: s.tokens}
	cfg := base
	cfg.Origin = s.origin
	client, err := mdrn.New(cfg)
	if err != nil {
		return nil, err
	}
	userEmail := "user_1@example.com"
	transcript := map[string]any{}
	scenario := func(name string, run func() (any, error)) {
		value, err := run()
		if err != nil {
			transcript[name] = failure(err)
			return
		}
		transcript[name] = value
	}
	headers := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(pairs); i += 2 {
			h.Set(pairs[i], pairs[i+1])
		}
		return h
	}
	requestURL := s.origin + "/account"
	plain := &http.Client{}

	// Database
	var one, two *mdrn.Record
	scenario("db.create", func() (any, error) {
		r, err := client.DB.Create(ctx, "tasks", map[string]any{"title": "one", "done": false}, mdrn.Key("create-one"))
		if err != nil {
			return nil, err
		}
		one = r
		return map[string]any{"collection": r.Collection, "version": r.Version, "data": data(r)}, nil
	})
	scenario("db.create.replay", func() (any, error) {
		r, err := client.DB.Create(ctx, "tasks", map[string]any{"title": "one", "done": false}, mdrn.Key("create-one"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"sameId": r.ID == one.ID, "version": r.Version}, nil
	})
	scenario("db.get", func() (any, error) {
		r, err := client.DB.Get(ctx, "tasks", one.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"found": r != nil, "version": r.Version, "data": data(r)}, nil
	})
	scenario("db.get.missing", func() (any, error) {
		r, err := client.DB.Get(ctx, "tasks", "rec_missing")
		if err != nil || r == nil {
			return nil, err
		}
		return r, nil
	})
	scenario("db.list.pages", func() (any, error) {
		r, err := client.DB.Create(ctx, "tasks", map[string]any{"title": "two", "done": false}, mdrn.Key("create-two"))
		if err != nil {
			return nil, err
		}
		two = r
		if _, err := client.DB.Create(ctx, "tasks", map[string]any{"title": "three", "done": false}, mdrn.Key("create-three")); err != nil {
			return nil, err
		}
		first, err := client.DB.List(ctx, "tasks", mdrn.PageOptions{Limit: 2})
		if err != nil {
			return nil, err
		}
		second, err := client.DB.List(ctx, "tasks", mdrn.PageOptions{Limit: 2, Cursor: first.NextCursor})
		if err != nil {
			return nil, err
		}
		titles := func(page *mdrn.Page[mdrn.Record]) []string {
			out := []string{}
			for _, r := range page.Items {
				var d struct {
					Title string `json:"title"`
				}
				_ = r.Decode(&d)
				out = append(out, d.Title)
			}
			return out
		}
		return map[string]any{"first": titles(first), "second": titles(second), "more": []bool{first.NextCursor != "", second.NextCursor != ""}}, nil
	})
	scenario("db.update.explicit", func() (any, error) {
		r, err := client.DB.Update(ctx, "tasks", one.ID, map[string]any{"done": true}, mdrn.AtVersion(1), mdrn.Key("update-one"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"version": r.Version, "data": data(r)}, nil
	})
	scenario("db.update.stale", func() (any, error) {
		return client.DB.Update(ctx, "tasks", one.ID, map[string]any{"done": false}, mdrn.AtVersion(1))
	})
	scenario("db.update.implicit", func() (any, error) {
		r, err := client.DB.Update(ctx, "tasks", one.ID, map[string]any{"title": "one!"})
		if err != nil {
			return nil, err
		}
		return map[string]any{"version": r.Version, "data": data(r)}, nil
	})
	scenario("db.update.contended.once", func() (any, error) {
		created, err := client.DB.Create(ctx, "contended-once", map[string]any{"n": 1}, mdrn.Key("create-contended-once"))
		if err != nil {
			return nil, err
		}
		r, err := client.DB.Update(ctx, "contended-once", created.ID, map[string]any{"n": 2})
		if err != nil {
			return nil, err
		}
		return map[string]any{"version": r.Version, "data": data(r)}, nil
	})
	scenario("db.update.contended", func() (any, error) {
		created, err := client.DB.Create(ctx, "contended", map[string]any{"n": 1}, mdrn.Key("create-contended"))
		if err != nil {
			return nil, err
		}
		return client.DB.Update(ctx, "contended", created.ID, map[string]any{"n": 2})
	})
	scenario("db.delete.explicit", func() (any, error) {
		if err := client.DB.Delete(ctx, "tasks", one.ID, mdrn.AtVersion(3), mdrn.Key("delete-one")); err != nil {
			return nil, err
		}
		r, err := client.DB.Get(ctx, "tasks", one.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"gone": r == nil}, nil
	})
	scenario("db.delete.implicit", func() (any, error) {
		four, err := client.DB.Create(ctx, "tasks", map[string]any{"title": "four", "done": false}, mdrn.Key("create-four"))
		if err != nil {
			return nil, err
		}
		if err := client.DB.Delete(ctx, "tasks", four.ID); err != nil {
			return nil, err
		}
		r, err := client.DB.Get(ctx, "tasks", four.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"gone": r == nil}, nil
	})
	scenario("db.delete.stale", func() (any, error) {
		return nil, client.DB.Delete(ctx, "tasks", two.ID, mdrn.AtVersion(5))
	})
	scenario("db.error.envelope", func() (any, error) {
		return client.DB.List(ctx, "tasks", mdrn.PageOptions{Limit: 1000})
	})
	scenario("db.error.noEnvelope", func() (any, error) {
		return client.DB.List(ctx, "broken", mdrn.PageOptions{})
	})
	scenario("db.error.redirect", func() (any, error) {
		return client.DB.List(ctx, "redirected", mdrn.PageOptions{})
	})

	// Client
	scenario("client.origin", func() (any, error) {
		construct := func(origin string) map[string]any {
			c := base
			c.Origin = origin
			_, err := mdrn.New(c)
			return attempt(err)
		}
		return map[string]any{"plainHttp": construct("http://app.example"), "loopbackHttp": construct("http://127.0.0.1:1"), "https": construct("https://app.example")}, nil
	})
	scenario("client.notEnabled", func() (any, error) {
		only, err := mdrn.New(mdrn.Config{Origin: s.origin, Project: s.project, Environment: s.environment, Tokens: mdrn.Tokens{Database: s.tokens.Database}})
		if err != nil {
			return nil, err
		}
		_, filesErr := only.Files.Get(ctx, "obj_x")
		_, mailErr := only.Mail.Send(ctx, mdrn.Message{To: userEmail, Subject: "Hi", Text: "Hello"})
		return map[string]any{"enabled": only.Enabled(), "files": attempt(filesErr), "mail": attempt(mailErr)}, nil
	})

	// Files
	view := func(f *mdrn.File) map[string]any {
		return map[string]any{"filename": f.Filename, "contentType": f.ContentType, "bytes": f.Bytes, "sha256": f.SHA256, "state": f.State}
	}
	var hello *mdrn.File
	scenario("files.upload.onePart", func() (any, error) {
		f, err := client.Files.Upload(ctx, mdrn.Upload{Name: "hello.txt", Type: "text/plain", Bytes: []byte("hello")}, mdrn.Key("upload-hello"))
		if err != nil {
			return nil, err
		}
		hello = f
		return view(f), nil
	})
	scenario("files.upload.twoParts", func() (any, error) {
		f, err := client.Files.Upload(ctx, mdrn.Upload{Name: "hello.bin", Bytes: []byte("hello, world")}, mdrn.Key("upload-world"))
		if err != nil {
			return nil, err
		}
		return view(f), nil
	})
	scenario("files.get", func() (any, error) {
		f, err := client.Files.Get(ctx, hello.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"found": f != nil, "state": f.State, "bytes": f.Bytes}, nil
	})
	scenario("files.get.missing", func() (any, error) {
		f, err := client.Files.Get(ctx, "obj_missing")
		if err != nil || f == nil {
			return nil, err
		}
		return f, nil
	})
	scenario("files.list", func() (any, error) {
		page, err := client.Files.List(ctx, mdrn.PageOptions{Limit: 10})
		if err != nil {
			return nil, err
		}
		names := []string{}
		for _, f := range page.Items {
			names = append(names, f.Filename)
		}
		return map[string]any{"filenames": names, "more": page.NextCursor != ""}, nil
	})
	scenario("files.downloadUrl", func() (any, error) {
		d, err := client.Files.DownloadURL(ctx, hello.ID, mdrn.Key("download-hello"))
		if err != nil {
			return nil, err
		}
		resp, err := plain.Get(d.URL)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return map[string]any{"urlUnderOrigin": strings.HasPrefix(d.URL, s.origin), "hasExpiresAt": !d.ExpiresAt.IsZero(), "downloaded": string(body)}, nil
	})
	scenario("files.delete", func() (any, error) {
		if err := client.Files.Delete(ctx, hello.ID, mdrn.Key("delete-hello")); err != nil {
			return nil, err
		}
		f, err := client.Files.Get(ctx, hello.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"gone": f == nil}, nil
	})
	scenario("files.uploads.manual", func() (any, error) {
		content := []byte("manual")
		digest := sha256.Sum256(content)
		created, err := client.Files.Uploads.Create(ctx, mdrn.UploadRequest{Filename: "manual.txt", ContentType: "text/plain", Bytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}, mdrn.Key("upload-manual"))
		if err != nil {
			return nil, err
		}
		polled, err := client.Files.Uploads.Get(ctx, created.ID)
		if err != nil {
			return nil, err
		}
		grant, err := client.Files.Uploads.Part(ctx, created.ID, 1, int64(len(content)), mdrn.Key("upload-manual-part-1"))
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, grant.Method, grant.URL, bytes.NewReader(content))
		if err != nil {
			return nil, err
		}
		for name, value := range grant.Headers {
			req.Header.Set(name, value)
		}
		put, err := plain.Do(req)
		if err != nil {
			return nil, err
		}
		defer put.Body.Close()
		var written struct {
			ETag string `json:"etag"`
		}
		if err := json.NewDecoder(put.Body).Decode(&written); err != nil {
			return nil, err
		}
		parts, err := client.Files.Uploads.Parts(ctx, created.ID, mdrn.PageOptions{})
		if err != nil {
			return nil, err
		}
		completion, err := client.Files.Uploads.Complete(ctx, created.ID, []mdrn.CompletedPart{{PartNumber: 1, ETag: written.ETag}}, mdrn.Key("upload-manual-complete"))
		if err != nil {
			return nil, err
		}
		operation, err := client.Files.Operation(ctx, completion.OperationID)
		if err != nil {
			return nil, err
		}
		file, err := client.Files.Get(ctx, completion.ID)
		if err != nil {
			return nil, err
		}
		uploads, err := client.Files.Uploads.List(ctx, mdrn.PageOptions{Limit: 10})
		if err != nil {
			return nil, err
		}
		numbers := []int{}
		for _, p := range parts.Items {
			numbers = append(numbers, p.PartNumber)
		}
		return map[string]any{"created": created.State, "polled": polled.State, "grantMethod": grant.Method, "putStatus": put.StatusCode, "parts": numbers, "completion": completion.State, "operation": operation.State, "file": file.State, "uploads": len(uploads.Items)}, nil
	})
	scenario("files.uploads.abort", func() (any, error) {
		created, err := client.Files.Uploads.Create(ctx, mdrn.UploadRequest{Filename: "abandoned.txt", ContentType: "text/plain", Bytes: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}, mdrn.Key("upload-abandoned"))
		if err != nil {
			return nil, err
		}
		if err := client.Files.Uploads.Abort(ctx, created.ID, mdrn.Key("abort-abandoned")); err != nil {
			return nil, err
		}
		session, err := client.Files.Uploads.Get(ctx, created.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"state": session.State}, nil
	})

	// Mail
	send := func(message mdrn.Message, key string) func() (any, error) {
		return func() (any, error) {
			sent, err := client.Mail.Send(ctx, message, mdrn.Key(key))
			if err != nil {
				return nil, err
			}
			return map[string]any{"state": sent.State}, nil
		}
	}
	scenario("mail.plain", send(mdrn.Message{To: userEmail, Subject: "Hi", Text: "Hello"}, "mail-plain"))
	scenario("mail.html", send(mdrn.Message{To: userEmail, Subject: "Hi", Text: "Hello", HTML: "<p>Hello</p>"}, "mail-html"))
	scenario("mail.template", send(mdrn.Message{To: userEmail, Template: "welcome"}, "mail-template"))
	scenario("mail.template.conflict", send(mdrn.Message{To: userEmail, Template: "welcome", Subject: "Hi"}, "mail-conflict"))
	scenario("mail.template.missing", send(mdrn.Message{To: userEmail, Template: "nope"}, "mail-missing"))

	// Auth
	signedIn := headers("x-mdrn-user-id", s.userID, "x-mdrn-identity-token", s.identityToken)
	user := func(h http.Header) func() (any, error) {
		return func() (any, error) {
			u := mdrn.UserFrom(h)
			if u == nil {
				return nil, nil
			}
			return map[string]any{"id": u.ID, "token": u.Token}, nil
		}
	}
	me := func(h http.Header) func() (any, error) {
		return func() (any, error) {
			p, err := client.Auth.Me(ctx, h, requestURL)
			if err != nil || p == nil {
				return nil, err
			}
			return p, nil
		}
	}
	scenario("auth.user.present", user(signedIn))
	scenario("auth.user.absent", user(headers()))
	scenario("auth.user.malformed", user(headers("x-mdrn-user-id", "not valid!", "x-mdrn-identity-token", s.identityToken)))
	scenario("auth.me", me(signedIn))
	scenario("auth.me.absent", me(headers()))
	scenario("auth.me.expired", me(headers("x-mdrn-user-id", s.userID, "x-mdrn-identity-token", "expired-token")))

	// Schedules
	run := func(h http.Header) func() (any, error) {
		return func() (any, error) {
			r := mdrn.ScheduledRunFrom(h)
			if r == nil {
				return nil, nil
			}
			return map[string]any{"schedule": r.Schedule, "runId": r.RunID, "scheduledTime": r.ScheduledTime.UTC().Format("2006-01-02T15:04:05Z"), "attempt": r.Attempt}, nil
		}
	}
	scenario("schedule.present", run(headers("x-mdrn-schedule", "digest", "x-mdrn-run-id", "run_1", "x-mdrn-scheduled-time", "2026-09-26T03:00:00Z", "x-mdrn-attempt", "2")))
	scenario("schedule.absent", run(headers()))

	return transcript, nil
}
