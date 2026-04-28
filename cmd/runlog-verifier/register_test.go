package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTestEnv installs a temp keystore path + scrubs RUNLOG_API_KEY and
// RUNLOG_API_URL so we never accidentally hit the real server during tests.
// Caller decides which env vars to set after.
func withTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "runlog", "key")
	t.Setenv("RUNLOG_KEY_PATH", keyPath)
	t.Setenv("RUNLOG_API_KEY", "")
	t.Setenv("RUNLOG_API_URL", "")
	return keyPath
}

func TestRegisterSuccess(t *testing.T) {
	keyPath := withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-deadbeef")

	var capturedAuth, capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/register-pubkey" {
			t.Errorf("expected /v1/register-pubkey, got %s", r.URL.Path)
		}
		capturedAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"registered","key_id":"aaaaaaaaaaaa"}`))
	}))
	defer srv.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", srv.URL},
		stdout, stderr,
	)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", rc, stderr.String())
	}

	// Wire-contract assertions on what the verifier sent.
	if capturedAuth != "Bearer sk-runlog-aaaaaaaaaaaa-deadbeef" {
		t.Errorf("unexpected Authorization header: %q", capturedAuth)
	}
	var bodyMap map[string]string
	if err := json.Unmarshal([]byte(capturedBody), &bodyMap); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, capturedBody)
	}
	if _, ok := bodyMap["public_key_b64"]; !ok {
		t.Errorf("body missing public_key_b64 field: %v", bodyMap)
	}

	// Stdout: machine-readable JSON envelope.
	var envelope map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, stdout.String())
	}
	if envelope["status"] != "registered" {
		t.Errorf("stdout.status = %q, want registered", envelope["status"])
	}
	if envelope["key_id"] != "aaaaaaaaaaaa" {
		t.Errorf("stdout.key_id = %q", envelope["key_id"])
	}
	if envelope["key_path"] != keyPath {
		t.Errorf("stdout.key_path = %q, want %q", envelope["key_path"], keyPath)
	}

	// Stderr: human-readable success line.
	if !strings.Contains(stderr.String(), "Registered key fingerprint") {
		t.Errorf("stderr missing fingerprint line: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "user@example.com") {
		t.Errorf("stderr missing email: %q", stderr.String())
	}

	// Key file exists on disk after a successful register.
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file not on disk: %v", err)
	}
}

func TestRegisterReusesExistingKey(t *testing.T) {
	keyPath := withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-deadbeef")

	// Pre-seed a key file via a first register call (against a 200 server).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"registered","key_id":"x"}`))
	}))
	defer srv.Close()

	stdout1 := &bytes.Buffer{}
	stderr1 := &bytes.Buffer{}
	if rc := runRegister(
		[]string{"--email", "u@e.com", "--server", srv.URL},
		stdout1, stderr1,
	); rc != 0 {
		t.Fatalf("first register failed: rc=%d stderr=%s", rc, stderr1.String())
	}
	first, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read first key: %v", err)
	}

	// Second register without --force must reuse the existing key (file
	// bytes unchanged) and still succeed.
	stdout2 := &bytes.Buffer{}
	stderr2 := &bytes.Buffer{}
	if rc := runRegister(
		[]string{"--email", "u@e.com", "--server", srv.URL},
		stdout2, stderr2,
	); rc != 0 {
		t.Fatalf("second register failed: rc=%d stderr=%s", rc, stderr2.String())
	}
	second, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read second key: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("key file changed across re-registrations without --force")
	}
}

func TestRegisterForceOverwrites(t *testing.T) {
	keyPath := withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-deadbeef")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"registered","key_id":"x"}`))
	}))
	defer srv.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runRegister(
		[]string{"--email", "u@e.com", "--server", srv.URL},
		stdout, stderr,
	); rc != 0 {
		t.Fatalf("first: rc=%d stderr=%s", rc, stderr.String())
	}
	before, _ := os.ReadFile(keyPath)

	stdout.Reset()
	stderr.Reset()
	if rc := runRegister(
		[]string{"--email", "u@e.com", "--server", srv.URL, "--force"},
		stdout, stderr,
	); rc != 0 {
		t.Fatalf("force: rc=%d stderr=%s", rc, stderr.String())
	}
	after, _ := os.ReadFile(keyPath)

	if bytes.Equal(before, after) {
		t.Errorf("--force did not generate a new key")
	}
}

func TestRegister409Conflict(t *testing.T) {
	withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-deadbeef")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"type":"pubkey_already_registered","message":"Use --force to rotate (not yet supported)."}}`))
	}))
	defer srv.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1 for 409, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "different pubkey") {
		t.Errorf("stderr should mention different pubkey: %q", stderr.String())
	}
}

func TestRegister401Unauthorized(t *testing.T) {
	withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-bogus")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"auth.invalid_key","message":"invalid or unknown API key"}}`))
	}))
	defer srv.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1 for 401, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "auth.invalid_key") {
		t.Errorf("stderr should surface error type: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "RUNLOG_API_KEY") {
		t.Errorf("stderr should hint at RUNLOG_API_KEY: %q", stderr.String())
	}
}

func TestRegister400BadRequest(t *testing.T) {
	withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-deadbeef")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"bad_request","message":"Ed25519 public keys are exactly 32 bytes; got 16"}}`))
	}))
	defer srv.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1 for 400, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "32 bytes") {
		t.Errorf("stderr should surface server message: %q", stderr.String())
	}
}

func TestRegisterNetworkError(t *testing.T) {
	withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-deadbeef")

	// Bind a port, immediately close the listener — connecting to it
	// will fail without the OS having to time out.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", "http://" + addr},
		stdout, stderr,
	)
	if rc != 2 {
		t.Fatalf("expected exit 2 for network error, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not reach") {
		t.Errorf("stderr should explain unreachability: %q", stderr.String())
	}
}

func TestRegisterMissingAPIKey(t *testing.T) {
	withTestEnv(t)
	// RUNLOG_API_KEY scrubbed by withTestEnv.

	// Fail the test if any HTTP call is made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("HTTP call made despite missing RUNLOG_API_KEY")
	}))
	defer srv.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "RUNLOG_API_KEY") {
		t.Errorf("stderr should explain missing env var: %q", stderr.String())
	}
}

func TestRegisterMissingEmail(t *testing.T) {
	withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-deadbeef")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister([]string{}, stdout, stderr)
	if rc != 1 {
		t.Fatalf("expected exit 1, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "--email") {
		t.Errorf("stderr should mention --email: %q", stderr.String())
	}
}

func TestRegisterServerEnvOverride(t *testing.T) {
	withTestEnv(t)
	t.Setenv("RUNLOG_API_KEY", "sk-runlog-aaaaaaaaaaaa-deadbeef")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"registered","key_id":"x"}`))
	}))
	defer srv.Close()
	t.Setenv("RUNLOG_API_URL", srv.URL)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com"},
		stdout, stderr,
	)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", rc, stderr.String())
	}
}
