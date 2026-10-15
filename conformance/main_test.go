package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	mdrn "github.com/mdrn-io/sdk-go"
	"github.com/mdrn-io/sdk-go/internal/testserver"
)

// The runner produces sdk/conformance/expected.json against the conformance server.
func TestConformance(t *testing.T) {
	origin := testserver.Start(t)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(testserver.Script()), "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	transcript, err := conformance(context.Background(), settings{
		origin: origin, project: testserver.Project, environment: testserver.Environment,
		tokens: mdrn.Tokens{Database: testserver.DatabaseToken, Storage: testserver.StorageToken, Mail: testserver.MailToken},
		userID: testserver.UserID, identityToken: testserver.IdentityToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Through JSON, so that typed values compare as the transcript prints them.
	printed, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err := json.Unmarshal(printed, &actual); err != nil {
		t.Fatal(err)
	}
	for name, want := range expected {
		if got, ok := actual[name]; !ok || !reflect.DeepEqual(got, want) {
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			t.Errorf("%s:\n  expected %s\n  actual   %s", name, wantJSON, gotJSON)
		}
	}
	for name := range actual {
		if _, ok := expected[name]; !ok {
			t.Errorf("%s is not a scenario of expected.json", name)
		}
	}
}
