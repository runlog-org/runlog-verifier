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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFullTestEnv installs temp paths for both the keystore and the
// config.json sink, scrubs RUNLOG_API_KEY and RUNLOG_API_URL, and
// installs a no-op browserOpenOverride so tests never spawn xdg-open.
// It also compresses the polling cadence so the loop completes in
// milliseconds. Returns (keyPath, configPath).
func withFullTestEnv(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "runlog", "key")
	configPath := filepath.Join(dir, "runlog", "config.json")
	t.Setenv("RUNLOG_KEY_PATH", keyPath)
	t.Setenv("RUNLOG_CONFIG_PATH", configPath)
	t.Setenv("RUNLOG_API_KEY", "")
	t.Setenv("RUNLOG_API_URL", "")

	// Default to "no-op browser open succeeded" so tests that don't pass
	// --no-browser still don't shell out. Tests that want to assert on
	// browser invocation override this themselves.
	prevBrowser := browserOpenOverride
	browserOpenOverride = func(string) error { return nil }
	t.Cleanup(func() { browserOpenOverride = prevBrowser })

	prevCadence := pollCadenceOverride
	pollCadenceOverride = []time.Duration{
		5 * time.Millisecond,
		5 * time.Millisecond,
	}
	t.Cleanup(func() { pollCadenceOverride = prevCadence })

	return keyPath, configPath
}

// fakeServer wires up a single httptest.NewServer with handlers for all
// three endpoints the verifier touches during register. Each handler is
// a closure the test can swap to inject the desired behavior; callers
// that don't need a particular endpoint can leave its handler nil and
// the mux will return 404.
type fakeServer struct {
	srv             *httptest.Server
	registerCLI     http.HandlerFunc
	statusHandler   http.HandlerFunc
	registerPubkey  http.HandlerFunc
	statusCallCount atomic.Int32
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/register-cli", func(w http.ResponseWriter, r *http.Request) {
		if fs.registerCLI == nil {
			http.NotFound(w, r)
			return
		}
		fs.registerCLI(w, r)
	})
	mux.HandleFunc("/register/status", func(w http.ResponseWriter, r *http.Request) {
		fs.statusCallCount.Add(1)
		if fs.statusHandler == nil {
			http.NotFound(w, r)
			return
		}
		fs.statusHandler(w, r)
	})
	mux.HandleFunc("/v1/register-pubkey", func(w http.ResponseWriter, r *http.Request) {
		if fs.registerPubkey == nil {
			http.NotFound(w, r)
			return
		}
		fs.registerPubkey(w, r)
	})
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

// kickoffOK installs a /v1/register-cli handler that returns a canned
// success response with verification_url pointing back at the same fake
// server (so verification_url passes validateServerURL).
func (fs *fakeServer) kickoffOK(t *testing.T, token string, expiresIn int) {
	t.Helper()
	fs.registerCLI = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("kickoff: expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("kickoff: should not send Authorization header, got %q", got)
		}
		var body struct {
			Email string `json:"email"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Email == "" {
			t.Errorf("kickoff: body.email empty")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"register_token":"` + token + `","verification_url":"` + fs.srv.URL + `/register/verify?token=` + token + `","expires_in_seconds":` + itoa(expiresIn) + `}`))
	}
}

// pubkeyOK installs a happy-path /v1/register-pubkey handler.
func (fs *fakeServer) pubkeyOK(t *testing.T, keyID string) {
	t.Helper()
	fs.registerPubkey = func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("pubkey: missing Bearer auth, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"registered","key_id":"` + keyID + `"}`))
	}
}

// statusVerified installs a /register/status handler that returns
// pending on the first call and verified on subsequent calls.
func (fs *fakeServer) statusPendingThenVerified(apiKey, keyID string) {
	var mu sync.Mutex
	served := 0
	fs.statusHandler = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		first := served == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			_, _ = w.Write([]byte(`{"status":"pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"verified","api_key":"` + apiKey + `","key_id":"` + keyID + `"}`))
	}
}

// itoa avoids pulling strconv only for these tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestRegisterHappyPath(t *testing.T) {
	keyPath, configPath := withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	fs.pubkeyOK(t, "kid-pubkey-xyz")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", rc, stderr.String())
	}

	// stdout: machine-readable JSON envelope with the new shape.
	var envelope map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, stdout.String())
	}
	if envelope["status"] != "registered" {
		t.Errorf("stdout.status = %q, want registered", envelope["status"])
	}
	if envelope["key_id"] != "kid-pubkey-xyz" {
		t.Errorf("stdout.key_id = %q", envelope["key_id"])
	}
	if envelope["api_key_id"] != "aaaaaaaaaaaa" {
		t.Errorf("stdout.api_key_id = %q", envelope["api_key_id"])
	}
	if envelope["key_path"] != keyPath {
		t.Errorf("stdout.key_path = %q, want %q", envelope["key_path"], keyPath)
	}
	if envelope["config_path"] != configPath {
		t.Errorf("stdout.config_path = %q, want %q", envelope["config_path"], configPath)
	}

	// stderr: human-readable success / fingerprint line.
	if !strings.Contains(stderr.String(), "Registered key fingerprint") {
		t.Errorf("stderr missing fingerprint line: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "user@example.com") {
		t.Errorf("stderr missing email: %q", stderr.String())
	}

	// Key file exists at the right path with mode 0600.
	if info, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file not on disk: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("key file mode = %o, want 0600", mode)
	}

	// Config file exists, mode 0600, contains the issued api_key.
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("config file not on disk: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config file mode = %o, want 0600", mode)
	}
	cfgBytes, _ := os.ReadFile(configPath)
	var cfg struct {
		APIKey   string `json:"api_key"`
		APIKeyID string `json:"api_key_id"`
	}
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		t.Fatalf("config not JSON: %v", err)
	}
	if cfg.APIKey != "sk-runlog-aaaaaaaaaaaa-deadbeef" {
		t.Errorf("config.api_key = %q", cfg.APIKey)
	}
	if cfg.APIKeyID != "aaaaaaaaaaaa" {
		t.Errorf("config.api_key_id = %q", cfg.APIKeyID)
	}

	// Status was polled at least twice (pending → verified).
	if got := fs.statusCallCount.Load(); got < 2 {
		t.Errorf("status polled %d times, want >= 2", got)
	}
}

func TestRegisterNoBrowserPrintsURL(t *testing.T) {
	withFullTestEnv(t)

	// Track whether the browser-open hook is called.
	browserCalled := false
	prev := browserOpenOverride
	browserOpenOverride = func(string) error {
		browserCalled = true
		return nil
	}
	t.Cleanup(func() { browserOpenOverride = prev })

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	fs.pubkeyOK(t, "kid-pubkey-xyz")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", fs.srv.URL, "--no-browser"},
		stdout, stderr,
	)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", rc, stderr.String())
	}
	if browserCalled {
		t.Errorf("--no-browser should not invoke the browser opener")
	}
	if !strings.Contains(stderr.String(), fs.srv.URL+"/register/verify?token=tok-abc") {
		t.Errorf("stderr should print verification URL: %q", stderr.String())
	}
	if !strings.Contains(strings.ToLower(stderr.String()), "browser") {
		t.Errorf("stderr should prompt about the browser: %q", stderr.String())
	}
}

func TestRegisterConfigConflictWithoutForceRefuses(t *testing.T) {
	_, configPath := withFullTestEnv(t)

	// Pre-seed config with a different api_key_id.
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"version":1,"api_key":"sk-existing","api_key_id":"existing-id","registered_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-newer-key", "different-id")
	// Pubkey handler intentionally absent — flow must abort before getting there.
	fs.registerPubkey = func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("pubkey upload should not be reached on config conflict")
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Errorf("stderr should mention --force: %q", stderr.String())
	}

	// Existing config preserved.
	got, _ := os.ReadFile(configPath)
	if !strings.Contains(string(got), `"existing-id"`) {
		t.Errorf("existing config was clobbered: %s", string(got))
	}
}

func TestRegisterConfigConflictForceOverwrites(t *testing.T) {
	_, configPath := withFullTestEnv(t)

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"version":1,"api_key":"sk-existing","api_key_id":"existing-id","registered_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-newer-key", "different-id")
	fs.pubkeyOK(t, "kid-pubkey-xyz")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", fs.srv.URL, "--force"},
		stdout, stderr,
	)
	if rc != 0 {
		t.Fatalf("expected exit 0 with --force, got %d. stderr=%s", rc, stderr.String())
	}

	// Config now reflects the new api_key_id.
	cfgBytes, _ := os.ReadFile(configPath)
	if !strings.Contains(string(cfgBytes), `"different-id"`) {
		t.Errorf("config not overwritten: %s", string(cfgBytes))
	}
}

func TestRegisterStatusVerifiedAlreadyClaimedFatal(t *testing.T) {
	_, configPath := withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"verified_already_claimed"}`))
	}
	fs.registerPubkey = func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("pubkey upload should not be reached on already_claimed")
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already claimed") {
		t.Errorf("stderr should explain already_claimed: %q", stderr.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("config should not have been written, got err=%v", err)
	}
}

func TestRegisterStatusExpiredFatal(t *testing.T) {
	withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"expired"}`))
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "expired") {
		t.Errorf("stderr should explain expired: %q", stderr.String())
	}
}

func TestRegisterKickoff400InvalidEmail(t *testing.T) {
	withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.registerCLI = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"register_cli.invalid_email","message":"email must be of the form local@domain"}}`))
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "not-an-email", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "email must be of the form") {
		t.Errorf("stderr should surface server message: %q", stderr.String())
	}
}

func TestRegisterKickoff429RateLimited(t *testing.T) {
	withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.registerCLI = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"too many","retry_after_seconds":42}}`))
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "42") {
		t.Errorf("stderr should surface retry_after_seconds: %q", stderr.String())
	}
}

func TestRegisterPollingTimeout(t *testing.T) {
	withFullTestEnv(t)

	fs := newFakeServer(t)
	// expires_in_seconds=1 + tight cadence forces the loop to deadline
	// almost immediately. Server stays "pending" forever.
	fs.kickoffOK(t, "tok-abc", 1)
	fs.statusHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"pending"}`))
	}

	// Override cadence with a generous-enough sleep that we definitely
	// hit the 1s deadline after a single iteration.
	prev := pollCadenceOverride
	pollCadenceOverride = []time.Duration{1100 * time.Millisecond}
	t.Cleanup(func() { pollCadenceOverride = prev })

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "user@example.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1 on timeout, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "timed out") {
		t.Errorf("stderr should explain timeout: %q", stderr.String())
	}
}

func TestRegisterPubkey409Conflict(t *testing.T) {
	// Failure-mode test for the pubkey-upload phase still applies even
	// though the kickoff/poll flow is now in front.
	withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	fs.registerPubkey = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"type":"pubkey_already_registered","message":"different pubkey on file"}}`))
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1 for 409, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "different pubkey") {
		t.Errorf("stderr should mention different pubkey: %q", stderr.String())
	}
}

func TestRegisterPubkey400BadRequest(t *testing.T) {
	withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	fs.registerPubkey = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"bad_request","message":"Ed25519 public keys are exactly 32 bytes; got 16"}}`))
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1 for 400, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "32 bytes") {
		t.Errorf("stderr should surface server message: %q", stderr.String())
	}
}

func TestRegisterReusesExistingKey(t *testing.T) {
	keyPath, _ := withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	fs.pubkeyOK(t, "kid-1")

	stdout1 := &bytes.Buffer{}
	stderr1 := &bytes.Buffer{}
	if rc := runRegister(
		[]string{"--email", "u@e.com", "--server", fs.srv.URL},
		stdout1, stderr1,
	); rc != 0 {
		t.Fatalf("first register failed: rc=%d stderr=%s", rc, stderr1.String())
	}
	first, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read first key: %v", err)
	}

	// Second register without --force must reuse the keypair (file bytes
	// unchanged) and still succeed.
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	stdout2 := &bytes.Buffer{}
	stderr2 := &bytes.Buffer{}
	if rc := runRegister(
		[]string{"--email", "u@e.com", "--server", fs.srv.URL},
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

func TestRegisterForceRegeneratesKey(t *testing.T) {
	keyPath, _ := withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	fs.pubkeyOK(t, "kid-1")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runRegister(
		[]string{"--email", "u@e.com", "--server", fs.srv.URL},
		stdout, stderr,
	); rc != 0 {
		t.Fatalf("first: rc=%d stderr=%s", rc, stderr.String())
	}
	before, _ := os.ReadFile(keyPath)

	// --force should generate a fresh keypair AND overwrite a pre-existing
	// (here: same-key_id) config — the same-key_id case would otherwise
	// be allowed without --force, but we want the keypair regen.
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	stdout.Reset()
	stderr.Reset()
	if rc := runRegister(
		[]string{"--email", "u@e.com", "--server", fs.srv.URL, "--force"},
		stdout, stderr,
	); rc != 0 {
		t.Fatalf("force: rc=%d stderr=%s", rc, stderr.String())
	}
	after, _ := os.ReadFile(keyPath)
	if bytes.Equal(before, after) {
		t.Errorf("--force did not generate a new keypair")
	}
}

func TestRegisterServerURLValidationRejectsNonLoopbackHTTP(t *testing.T) {
	withFullTestEnv(t)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", "http://example.com"},
		stdout, stderr,
	)
	if rc != 1 {
		t.Fatalf("expected exit 1, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "loopback") {
		t.Errorf("stderr should mention loopback restriction: %q", stderr.String())
	}
}

func TestRegisterServerEnvOverride(t *testing.T) {
	withFullTestEnv(t)

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa")
	fs.pubkeyOK(t, "kid-1")
	t.Setenv("RUNLOG_API_URL", fs.srv.URL)

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

func TestRegisterMissingEmail(t *testing.T) {
	withFullTestEnv(t)

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

func TestRegisterKickoffNetworkError(t *testing.T) {
	withFullTestEnv(t)

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

// TestRegisterDoesNotRequireAPIKeyEnv covers the headline UX win of F73:
// a fresh user with no RUNLOG_API_KEY in their shell can run register
// and still come out the other side with a usable key on disk.
func TestRegisterDoesNotRequireAPIKeyEnv(t *testing.T) {
	_, configPath := withFullTestEnv(t)

	// Sanity-check that the env IS empty (withFullTestEnv clears it).
	if os.Getenv("RUNLOG_API_KEY") != "" {
		t.Fatalf("test setup leaked RUNLOG_API_KEY")
	}

	fs := newFakeServer(t)
	fs.kickoffOK(t, "tok-abc", 60)
	fs.statusPendingThenVerified("sk-runlog-aaaaaaaaaaaa-fresh", "fresh-id")
	fs.pubkeyOK(t, "kid-1")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runRegister(
		[]string{"--email", "u@e.com", "--server", fs.srv.URL},
		stdout, stderr,
	)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", rc, stderr.String())
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("config not written: %v", err)
	}
}

// Ensure the test helper itself compiles without unused imports if we
// later drop tests above.
var _ = io.Discard
