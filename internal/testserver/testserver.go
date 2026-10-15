// Package testserver starts the SDK conformance server (sdk/conformance/server.mjs, the fake
// MDRN of @mdrnio/sdk/fake behind node:http) for a test, and gives its origin. The server is
// part of the monorepo, not of the published module: a test that needs it is skipped where
// the file or node is missing.
package testserver

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fixed facts of the conformance server.
const (
	Project       = "project_one"
	Environment   = "sandbox"
	DatabaseToken = "database-token"
	StorageToken  = "storage-token"
	MailToken     = "mail-token"
	UserID        = "user_1"
	UserEmail     = "user_1@example.com"
	IdentityToken = "identity-token-1"
	PartBytes     = 8
)

// Script is the server's path, found from this file's.
func Script() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "conformance", "server.mjs")
}

// Start runs the server for the test and answers its origin (http://127.0.0.1:<port>). The
// server is stopped when the test ends.
func Start(t testing.TB) string {
	t.Helper()
	script := Script()
	if _, err := os.Stat(script); err != nil {
		t.Skipf("the conformance server is not here (%s): the published module has no fake MDRN", script)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the conformance server needs it")
	}
	cmd := exec.Command(node, script)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	port, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the conformance server did not print its port: %v", err)
	}
	return "http://127.0.0.1:" + strings.TrimSpace(port)
}
