package main

import (
	"context"
	"testing"

	mdrn "github.com/mdrn-io/sdk-go"
	"github.com/mdrn-io/sdk-go/internal/testserver"
)

func TestRoundTrip(t *testing.T) {
	origin := testserver.Start(t)
	client, err := mdrn.New(mdrn.Config{
		Origin: origin, Project: testserver.Project, Environment: testserver.Environment,
		Tokens: mdrn.Tokens{Database: testserver.DatabaseToken, Storage: testserver.StorageToken, Mail: testserver.MailToken},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := roundTrip(context.Background(), client, testserver.UserEmail)
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.ID == "" || len(result.Record.Versions) != 2 || result.Record.Versions[0] != 1 || result.Record.Versions[1] != 2 || !result.Record.Deleted {
		t.Fatalf("record %+v", result.Record)
	}
	if result.File.ID == "" || result.File.Bytes != int64(len(content)) || result.File.State != "ready" || result.File.Downloaded != string(content) || !result.File.Deleted {
		t.Fatalf("file %+v", result.File)
	}
	if !result.Mail.Sent || result.Mail.State != "queued" {
		t.Fatalf("mail %+v", result.Mail)
	}
}
