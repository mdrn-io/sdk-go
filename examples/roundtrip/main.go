// Command roundtrip uses most of the SDK once: a Database record (create, read, update, delete),
// a file (upload, download through a plain URL, delete) and, when Mail is enabled and
// MDRN_MAIL_TO is set, one message.
//
// Run it with the variables MDRN gives a release (or the values of your own):
//
//	MDRN_PROJECT_ID=... MDRN_ENVIRONMENT=sandbox \
//	MDRN_DATABASE_URL=... MDRN_DATABASE_TOKEN=... MDRN_STORAGE_URL=... MDRN_STORAGE_TOKEN=... \
//	MDRN_MAIL_URL=... MDRN_MAIL_TOKEN=... MDRN_MAIL_TO=you@example.com go run ./examples/roundtrip
//
// main_test.go runs it against the SDK's fake MDRN.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	mdrn "github.com/mdrn-io/sdk-go"
)

var content = []byte("Hello from the MDRN Go SDK\n")

// Result is what the example did.
type Result struct {
	Record struct {
		ID       string   `json:"id"`
		Versions []uint64 `json:"versions"`
		Deleted  bool     `json:"deleted"`
	} `json:"record"`
	File struct {
		ID         string `json:"id"`
		Bytes      int64  `json:"bytes"`
		State      string `json:"state"`
		Downloaded string `json:"downloaded"`
		Deleted    bool   `json:"deleted"`
	} `json:"file"`
	Mail struct {
		Sent  bool   `json:"sent"`
		State string `json:"state,omitempty"`
	} `json:"mail"`
}

func main() {
	client, err := mdrn.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	result, err := roundTrip(context.Background(), client, os.Getenv("MDRN_MAIL_TO"))
	if err != nil {
		log.Fatal(err)
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))
}

// roundTrip does the work; mailTo is who gets the message, nobody when empty.
func roundTrip(ctx context.Context, client *mdrn.Client, mailTo string) (*Result, error) {
	result := &Result{}

	note, err := client.DB.Create(ctx, "notes", map[string]any{"text": "hello"})
	if err != nil {
		return nil, err
	}
	read, err := client.DB.Get(ctx, "notes", note.ID)
	if err != nil {
		return nil, err
	}
	if read == nil {
		return nil, fmt.Errorf("the note %s is gone right after its creation", note.ID)
	}
	// The user saw version read.Version, so the change is sent with it; a CONFLICT would mean
	// somebody else changed the note in between. Without AtVersion the SDK would read and
	// retry on its own.
	updated, err := client.DB.Update(ctx, "notes", note.ID, map[string]any{"text": "hello again"}, mdrn.AtVersion(read.Version))
	if err != nil {
		return nil, err
	}
	if err := client.DB.Delete(ctx, "notes", note.ID); err != nil {
		return nil, err
	}
	result.Record.ID, result.Record.Versions, result.Record.Deleted = note.ID, []uint64{note.Version, updated.Version}, true

	file, err := client.Files.Upload(ctx, mdrn.Upload{Name: "hello.txt", Type: "text/plain", Bytes: content})
	if err != nil {
		return nil, err
	}
	download, err := client.Files.DownloadURL(ctx, file.ID)
	if err != nil {
		return nil, err
	}
	// The URL is the whole credential: a plain GET, no token.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, download.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	downloaded, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the download answered HTTP %d", resp.StatusCode)
	}
	if err := client.Files.Delete(ctx, file.ID); err != nil {
		return nil, err
	}
	result.File.ID, result.File.Bytes, result.File.State, result.File.Downloaded, result.File.Deleted = file.ID, file.Bytes, file.State, string(downloaded), true

	if mailTo != "" {
		sent, err := client.Mail.Send(ctx, mdrn.Message{To: mailTo, Subject: "Round trip", Text: "The MDRN Go SDK example ran."})
		if err != nil {
			return nil, err
		}
		result.Mail.Sent, result.Mail.State = true, sent.State
	}
	return result, nil
}
