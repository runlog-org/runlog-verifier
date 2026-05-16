package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"github.com/runlog-org/runlog-verifier/internal/clientconfig"
	"github.com/runlog-org/runlog-verifier/internal/keystore"
)

const (
	defaultRegisterServer = "https://api.runlog.org"
	registerPath          = "/v1/register-pubkey"
	registerCLIPath       = "/v1/register-cli"
	registerStatusPath    = "/register/status"
	registerHTTPTimeout   = 10 * time.Second
	// maxResponseBytes caps how much of an HTTP response body we will read
	// into memory. The register endpoint emits small JSON envelopes; a
	// hostile or misconfigured server could otherwise stream gigabytes
	// into the verifier and OOM it.
	maxResponseBytes = 1 << 20

	// pollMaxTotalSeconds caps the polling loop wall-clock at 10 min — even
	// if the server says expires_in_seconds is larger, we don't want a
	// runaway CLI in CI.
	pollMaxTotalSeconds = 600
)

// registerExitOK / registerExitUser / registerExitNet keep the exit-code
// mapping legible inside the handler. Aligned with the verifier's
// existing exit-code conventions documented in usage().
const (
	registerExitOK   = 0
	registerExitUser = 1 // missing args, 401/400/409, refusal to overwrite, fatal poll states
	registerExitNet  = 2 // network error, unexpected status, internal failure
)

// registerHTTPClient is overridable in tests; nil means use a fresh
// http.Client with the registerHTTPTimeout.
var registerHTTPClient *http.Client

// httpClient returns the test-injected registerHTTPClient when set, else a
// fresh http.Client bounded by registerHTTPTimeout. Centralised so the
// three register HTTP call sites (kickoff, status poll, pubkey upload) don't
// each hand-roll the same nil-check + timeout-client construction.
func httpClient() *http.Client {
	if registerHTTPClient != nil {
		return registerHTTPClient
	}
	return &http.Client{Timeout: registerHTTPTimeout}
}

// pollCadenceOverride lets tests bypass the real exponential backoff so
// the polling loop completes in milliseconds rather than seconds. nil
// means use the production cadence (2s, 2s, 3s, 5s, 5s, …).
var pollCadenceOverride []time.Duration

// browserOpenOverride lets tests intercept the would-be xdg-open / open
// invocation without spawning a real subprocess. The override receives the
// URL and returns nil on success; if non-nil, the URL is *not* passed to
// the real os/exec path.
var browserOpenOverride func(url string) error

// runRegister implements the `register` subcommand. stdout/stderr are
// passed in (rather than hardcoded to os.Stdout/Stderr) so tests can
// capture output without juggling pipes around the real fds.
func runRegister(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		emailFlag     = fs.String("email", "", "email address to associate with this key (required)")
		forceFlag     = fs.Bool("force", false, "overwrite an existing local keypair AND/OR an existing config.json with a different api_key_id")
		serverFlag    = fs.String("server", "", "registration server base URL (default: $RUNLOG_API_URL or "+defaultRegisterServer+")")
		noBrowserFlag = fs.Bool("no-browser", false, "do not auto-open the verification URL; print it to stderr instead")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: runlog-verifier register --email <addr> [--force] [--no-browser] [--server <url>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return registerExitUser
	}
	if *emailFlag == "" {
		fmt.Fprintln(stderr, "register: --email is required")
		fs.Usage()
		return registerExitUser
	}

	// Same override > $RUNLOG_API_URL > defaultRegisterServer cascade the
	// verify subcommand uses for its pre-flight; share the one resolver
	// rather than hand-rolling it in two places.
	server := resolveServerURL(*serverFlag)

	// Validate the base server URL once via a probe parse of the kickoff
	// endpoint; the same scheme/host applies to every subsequent call so
	// we don't need to re-validate per-endpoint.
	probe, err := url.Parse(server + registerCLIPath)
	if err != nil {
		fmt.Fprintf(stderr, "register: invalid server URL %q: %v\n", server, err)
		return registerExitUser
	}
	if err := validateServerURL(probe); err != nil {
		fmt.Fprintf(stderr,
			"register: --server / RUNLOG_API_URL %q rejected: %v\n",
			server, err)
		return registerExitUser
	}

	// 1. Kickoff: POST /v1/register-cli with {email}.
	kickoff, rc := postRegisterCLI(server, *emailFlag, stderr)
	if rc != registerExitOK {
		return rc
	}

	// 2. Open the verification URL (or print it) so the user can confirm.
	if *noBrowserFlag {
		fmt.Fprintf(stderr,
			"\n  Open this URL in your browser to verify the registration:\n\n    %s\n\n",
			kickoff.VerificationURL)
	} else {
		if err := openBrowser(kickoff.VerificationURL); err != nil {
			// Browser open is best-effort — fall back to the print path
			// so an unattended/headless box still completes the flow.
			fmt.Fprintf(stderr,
				"\n  Could not open browser automatically (%v).\n"+
					"  Open this URL in your browser to verify the registration:\n\n    %s\n\n",
				err, kickoff.VerificationURL)
		} else {
			fmt.Fprintf(stderr,
				"\n  Opened %s in your browser. Click the verification link there to continue.\n\n",
				kickoff.VerificationURL)
		}
	}

	// 3. Poll until verified / expired / fatal.
	pollResult, rc := pollRegisterStatus(server, kickoff.RegisterToken, kickoff.ExpiresInSeconds, stderr)
	if rc != registerExitOK {
		return rc
	}

	// 4. Persist the API key to ~/.runlog/config.json BEFORE we touch the
	// keystore — if the keystore step fails we still want a record of the
	// issued credential so the user can recover without burning a fresh
	// kickoff token.
	if err := clientconfig.Save(pollResult.APIKey, pollResult.KeyID, *forceFlag); err != nil {
		if errors.Is(err, clientconfig.ErrAlreadyExists) {
			path, _ := clientconfig.Path()
			fmt.Fprintf(stderr,
				"register: %s already exists with a different api_key_id. "+
					"Re-run with --force to overwrite (this discards the previously "+
					"issued API key — make sure you don't need it).\n", path)
			return registerExitUser
		}
		fmt.Fprintf(stderr, "register: persist config: %v\n", err)
		return registerExitNet
	}
	configPath, _ := clientconfig.Path()

	// 5. Load or generate the local Ed25519 keypair, then upload the pubkey
	// using the existing register-pubkey endpoint and helper.
	_, pub, keyPath, err := loadOrGenerateKey(*forceFlag)
	if err != nil {
		if errors.Is(err, keystore.ErrAlreadyExists) {
			fmt.Fprintln(stderr,
				"register: a keypair already exists at "+keyPath+". "+
					"Re-run with --force to overwrite (only do this if you've "+
					"verified the existing key is unrecoverable — the server "+
					"will reject any new pubkey under this API key with HTTP 409).")
			return registerExitUser
		}
		fmt.Fprintf(stderr, "register: %v\n", err)
		return registerExitNet
	}

	pubkeyEndpoint := server + registerPath
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	resp, body, err := postRegisterPubkey(pubkeyEndpoint, pollResult.APIKey, pubB64)
	if err != nil {
		fmt.Fprintf(stderr, "register: could not reach %s: %v\n", server, err)
		return registerExitNet
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var ok struct {
			Status string `json:"status"`
			KeyID  string `json:"key_id"`
		}
		if err := json.Unmarshal(body, &ok); err != nil {
			fmt.Fprintf(stderr, "register: server returned 200 but body was not parseable: %v\n", err)
			return registerExitNet
		}
		fingerprintHex := pubFingerprint(pub)
		fmt.Fprintf(stderr, "Registered key fingerprint %s for %s\n", fingerprintHex, *emailFlag)
		out := map[string]string{
			"status":      "registered",
			"key_id":      ok.KeyID,
			"api_key_id":  pollResult.KeyID,
			"key_path":    keyPath,
			"config_path": configPath,
		}
		if err := encodeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "register: encode output: %v\n", err)
			return registerExitNet
		}
		return registerExitOK

	case http.StatusConflict: // 409
		fmt.Fprintln(stderr,
			"register: a different pubkey is already registered for this API key. "+
				"Pubkey rotation is not yet supported. Contact support to rotate.")
		return registerExitUser

	case http.StatusUnauthorized: // 401
		errType, _ := serverErrorType(body)
		if errType == "" {
			errType = "unknown"
		}
		fmt.Fprintf(stderr,
			"register: pubkey upload failed authentication: %s. "+
				"This is unexpected immediately after a fresh API key issue.\n",
			errType)
		return registerExitUser

	case http.StatusBadRequest: // 400
		_, msg := serverErrorType(body)
		if msg == "" {
			msg = string(body)
		}
		fmt.Fprintf(stderr, "register: server rejected the public key: %s\n", msg)
		return registerExitUser

	default:
		fmt.Fprintf(stderr,
			"register: unexpected response status %d from %s\nbody: %s\n",
			resp.StatusCode, server, string(body))
		return registerExitNet
	}
}

// kickoffResponse is the parsed body of POST /v1/register-cli.
type kickoffResponse struct {
	RegisterToken    string `json:"register_token"`
	VerificationURL  string `json:"verification_url"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

// postRegisterCLI requests a fresh CLI registration token from the server.
// Returns (response, exitCode); exitCode is registerExitOK on success,
// otherwise it is the exit code the caller should return.
func postRegisterCLI(server, email string, stderr io.Writer) (kickoffResponse, int) {
	endpoint := server + registerCLIPath
	payload, err := json.Marshal(map[string]string{"email": email})
	if err != nil {
		fmt.Fprintf(stderr, "register: marshal kickoff body: %v\n", err)
		return kickoffResponse{}, registerExitNet
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(stderr, "register: build kickoff request: %v\n", err)
		return kickoffResponse{}, registerExitNet
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "register: could not reach %s: %v\n", server, err)
		return kickoffResponse{}, registerExitNet
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxResponseBytes))
	if err != nil {
		fmt.Fprintf(stderr, "register: read kickoff response body: %v\n", err)
		return kickoffResponse{}, registerExitNet
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var k kickoffResponse
		if err := json.Unmarshal(body, &k); err != nil {
			fmt.Fprintf(stderr, "register: kickoff returned 200 but body was not parseable: %v\n", err)
			return kickoffResponse{}, registerExitNet
		}
		if k.RegisterToken == "" || k.VerificationURL == "" {
			fmt.Fprintf(stderr,
				"register: kickoff returned 200 but body was missing required fields (token=%q url=%q)\n",
				k.RegisterToken, k.VerificationURL)
			return kickoffResponse{}, registerExitNet
		}
		// Cap any over-eager expires_in_seconds at the configured ceiling
		// so a server quirk can't extend the local poll wall-clock past
		// pollMaxTotalSeconds.
		if k.ExpiresInSeconds <= 0 {
			k.ExpiresInSeconds = pollMaxTotalSeconds
		}
		// Validate the verification URL the same way as the server URL —
		// we may hand it to xdg-open shortly. Refuse non-https / non-loopback.
		vu, err := url.Parse(k.VerificationURL)
		if err != nil {
			fmt.Fprintf(stderr, "register: server returned malformed verification_url %q: %v\n", k.VerificationURL, err)
			return kickoffResponse{}, registerExitNet
		}
		if err := validateServerURL(vu); err != nil {
			fmt.Fprintf(stderr,
				"register: server returned untrusted verification_url %q: %v\n",
				k.VerificationURL, err)
			return kickoffResponse{}, registerExitNet
		}
		return k, registerExitOK

	case http.StatusBadRequest:
		_, msg := serverErrorType(body)
		if msg == "" {
			msg = string(body)
		}
		fmt.Fprintf(stderr, "register: server rejected --email: %s\n", msg)
		return kickoffResponse{}, registerExitUser

	case http.StatusTooManyRequests:
		if secs := retryAfterSeconds(body); secs > 0 {
			fmt.Fprintf(stderr,
				"register: rate-limited by server, retry in %ds\n", secs)
		} else {
			fmt.Fprintln(stderr, "register: rate-limited by server, retry shortly")
		}
		return kickoffResponse{}, registerExitUser

	case http.StatusServiceUnavailable:
		fmt.Fprintln(stderr, "register: server temporarily unavailable, retry shortly")
		return kickoffResponse{}, registerExitNet

	default:
		fmt.Fprintf(stderr,
			"register: unexpected kickoff status %d from %s\nbody: %s\n",
			resp.StatusCode, server, string(body))
		return kickoffResponse{}, registerExitNet
	}
}

// statusResponse is the parsed body of GET /register/status.
type statusResponse struct {
	Status string `json:"status"`
	APIKey string `json:"api_key,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

// pollCadence returns the production poll-sleep-cadence (2s, 2s, 3s, then
// 5s indefinitely). Tests can swap in pollCadenceOverride to compress this
// to milliseconds.
func pollCadence(attempt int) time.Duration {
	if pollCadenceOverride != nil {
		if attempt < len(pollCadenceOverride) {
			return pollCadenceOverride[attempt]
		}
		return pollCadenceOverride[len(pollCadenceOverride)-1]
	}
	switch {
	case attempt < 2:
		return 2 * time.Second
	case attempt == 2:
		return 3 * time.Second
	default:
		return 5 * time.Second
	}
}

// pollRegisterStatus loops on GET /register/status?token=… until the
// server reports verified, expired, or another fatal state, or the wall
// clock hits min(expiresInSeconds, pollMaxTotalSeconds). Returns
// (response, exitCode).
func pollRegisterStatus(server, token string, expiresInSeconds int, stderr io.Writer) (statusResponse, int) {
	endpoint := server + registerStatusPath + "?token=" + url.QueryEscape(token)

	maxSecs := expiresInSeconds
	if maxSecs <= 0 || maxSecs > pollMaxTotalSeconds {
		maxSecs = pollMaxTotalSeconds
	}
	deadline := time.Now().Add(time.Duration(maxSecs) * time.Second)

	fmt.Fprintln(stderr, "Waiting for verification...")

	client := httpClient()

	attempt := 0
	// Track when we last printed a liveness dot so the user sees a tick
	// roughly every 10s without spam.
	lastDotAt := time.Now()
	dotInterval := 10 * time.Second

	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "\nregister: verification timed out — the kickoff token expired before the URL was clicked. Re-run 'runlog-verifier register --email <you>' to retry.")
			return statusResponse{}, registerExitUser
		}

		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			fmt.Fprintf(stderr, "\nregister: build status request: %v\n", err)
			return statusResponse{}, registerExitNet
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(stderr, "\nregister: could not reach %s: %v\n", server, err)
			return statusResponse{}, registerExitNet
		}
		body, readErr := io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			fmt.Fprintf(stderr, "\nregister: read status body: %v\n", readErr)
			return statusResponse{}, registerExitNet
		}

		switch resp.StatusCode {
		case http.StatusOK:
			var s statusResponse
			if err := json.Unmarshal(body, &s); err != nil {
				fmt.Fprintf(stderr, "\nregister: status returned 200 but body was not parseable: %v\n", err)
				return statusResponse{}, registerExitNet
			}
			switch s.Status {
			case "pending":
				// keep polling
			case "verified":
				if s.APIKey == "" || s.KeyID == "" {
					fmt.Fprintf(stderr, "\nregister: status reported verified but did not include api_key/key_id\n")
					return statusResponse{}, registerExitNet
				}
				fmt.Fprintln(stderr, "\nVerified.")
				return s, registerExitOK
			case "verified_already_claimed":
				fmt.Fprintln(stderr, "\nregister: verification was confirmed but a previous poll already claimed the API key. The issued credential is no longer reachable from this CLI. Re-run 'runlog-verifier register --email <you>' to start a fresh registration.")
				return statusResponse{}, registerExitUser
			case "expired":
				fmt.Fprintln(stderr, "\nregister: kickoff token expired before verification completed. Re-run 'runlog-verifier register --email <you>' to retry.")
				return statusResponse{}, registerExitUser
			default:
				fmt.Fprintf(stderr, "\nregister: unexpected status %q from server\n", s.Status)
				return statusResponse{}, registerExitNet
			}

		case http.StatusNotFound:
			errType, msg := serverErrorType(body)
			if errType == "" {
				errType = "unknown"
			}
			if msg == "" {
				msg = "unknown register token"
			}
			fmt.Fprintf(stderr, "\nregister: server returned 404 (%s): %s. Re-run 'runlog-verifier register --email <you>' to retry.\n", errType, msg)
			return statusResponse{}, registerExitUser

		case http.StatusBadRequest:
			errType, msg := serverErrorType(body)
			if errType == "" {
				errType = "bad_request"
			}
			if msg == "" {
				msg = string(body)
			}
			fmt.Fprintf(stderr, "\nregister: server returned 400 (%s): %s\n", errType, msg)
			return statusResponse{}, registerExitUser

		case http.StatusTooManyRequests:
			wait := time.Duration(retryAfterSeconds(body)) * time.Second
			if wait <= 0 {
				wait = 5 * time.Second
			}
			// Don't honor a retry-after that pushes us past the deadline —
			// fall through to the timeout branch on the next iteration.
			if time.Now().Add(wait).After(deadline) {
				fmt.Fprintln(stderr, "\nregister: rate-limited beyond the kickoff token's lifetime. Re-run 'runlog-verifier register --email <you>' to retry.")
				return statusResponse{}, registerExitUser
			}
			sleepWithDots(stderr, wait, &lastDotAt, dotInterval)
			continue

		case http.StatusServiceUnavailable:
			// Treat as transient — keep polling on the same cadence.

		default:
			fmt.Fprintf(stderr,
				"\nregister: unexpected status response %d from %s\nbody: %s\n",
				resp.StatusCode, server, string(body))
			return statusResponse{}, registerExitNet
		}

		// Sleep before the next attempt.
		wait := pollCadence(attempt)
		if time.Now().Add(wait).After(deadline) {
			// One short final wait so we still poll once at the boundary.
			wait = time.Until(deadline)
			if wait < 0 {
				wait = 0
			}
		}
		sleepWithDots(stderr, wait, &lastDotAt, dotInterval)
		attempt++
	}
}

// sleepWithDots sleeps for total in chunks, printing a "." to stderr
// roughly every dotInterval since lastDotAt. lastDotAt is updated in
// place so successive sleeps interleave cleanly. When pollCadenceOverride
// is non-nil (i.e. tests), we just sleep and skip the dot ticking — tests
// would otherwise see noisy output.
func sleepWithDots(stderr io.Writer, total time.Duration, lastDotAt *time.Time, dotInterval time.Duration) {
	if total <= 0 {
		return
	}
	if pollCadenceOverride != nil {
		time.Sleep(total)
		return
	}
	end := time.Now().Add(total)
	for time.Now().Before(end) {
		chunk := time.Until(end)
		if chunk > time.Second {
			chunk = time.Second
		}
		time.Sleep(chunk)
		if time.Since(*lastDotAt) >= dotInterval {
			fmt.Fprint(stderr, ".")
			*lastDotAt = time.Now()
		}
	}
}

// openBrowser launches the platform's default browser pointed at url.
// Best-effort: returns nil on Start() success without waiting for the
// child process to exit (the user might be on a headless box without a
// desktop session). On unsupported GOOS, returns an error so the caller
// can fall back to printing the URL.
func openBrowser(target string) error {
	if browserOpenOverride != nil {
		return browserOpenOverride(target)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		return fmt.Errorf("unsupported GOOS %q", runtime.GOOS)
	}
	// Detach stdio — xdg-open / open are typically silent but we don't
	// want any chatter polluting our stderr.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Stdin = nil
	return cmd.Start()
}

// validateServerURL guards against accidental or hostile --server /
// RUNLOG_API_URL values that would otherwise smuggle the API key into
// non-HTTPS endpoints (or non-HTTP schemes entirely). HTTPS is always
// allowed; plain HTTP is allowed only against loopback (127.0.0.1, ::1,
// localhost) so test fixtures (httptest.NewServer) keep working without
// opening a TLS-stripping path against arbitrary hosts.
func validateServerURL(u *url.URL) error {
	if u.Host == "" {
		return errors.New("missing host")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		switch host {
		case "127.0.0.1", "::1", "localhost":
			return nil
		}
		return fmt.Errorf("scheme http is only allowed against loopback (got host %q)", host)
	default:
		return fmt.Errorf("scheme %q is not allowed (use https, or http to loopback)", u.Scheme)
	}
}

// loadOrGenerateKey returns (priv, pub, path, err). If the key file
// already exists and force=false, it loads it (this is the documented
// "register on a machine that already has a key" path — we still upload
// the pubkey so re-running register on a fresh API key works). If force=true
// it always generates and overwrites.
//
// When force=false and the file is missing, we generate + save fresh.
// When force=true and an existing file is corrupt, we still want to
// overwrite — so we don't try to Load first in that case.
func loadOrGenerateKey(force bool) (ed25519.PrivateKey, ed25519.PublicKey, string, error) {
	path, err := keystore.Path()
	if err != nil {
		return nil, nil, "", err
	}

	if !force {
		priv, pub, err := keystore.Load()
		switch {
		case err == nil:
			// Existing key — reuse it. Save() with force=false would
			// raise ErrAlreadyExists, but we have no reason to call it.
			return priv, pub, path, nil
		case errors.Is(err, keystore.ErrNotFound):
			// fall through to generate
		default:
			return nil, nil, path, fmt.Errorf("load existing key: %w", err)
		}
	}

	priv, pub, err := keystore.Generate()
	if err != nil {
		return nil, nil, path, err
	}
	if err := keystore.Save(priv, pub, force); err != nil {
		return nil, nil, path, err
	}
	return priv, pub, path, nil
}

// postRegisterPubkey performs the HTTP POST and returns (resp, body, err).
// The response body is read fully and returned alongside resp so callers
// can JSON-decode it without worrying about exhaust order.
func postRegisterPubkey(endpoint, apiKey, pubB64 string) (*http.Response, []byte, error) {
	payload, err := json.Marshal(map[string]string{"public_key_b64": pubB64})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxResponseBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("read response body: %w", err)
	}
	return resp, body, nil
}

// serverErrorType extracts {"error": {"type": "...", "message": "..."}}
// from the response body. The server uses this nested shape on every
// non-2xx (verified in runlog/server/src/runlog/auth/pubkey_routes.py).
// Returns ("", "") if the body doesn't match.
func serverErrorType(body []byte) (string, string) {
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", ""
	}
	return envelope.Error.Type, envelope.Error.Message
}

// retryAfterSeconds extracts {"error": {"retry_after_seconds": N}} from a
// 429 response body. Returns 0 when the field is absent or the body does
// not parse — callers treat a non-positive value as "no server-supplied
// hint" and fall back to their own default cadence. Shared by every 429
// arm (kickoff, status poll, pre-flight) so the envelope shape lives once.
func retryAfterSeconds(body []byte) int {
	var withRetry struct {
		Error struct {
			RetryAfterSeconds int `json:"retry_after_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &withRetry); err != nil {
		return 0
	}
	return withRetry.Error.RetryAfterSeconds
}

// pubFingerprint returns the first 12 hex chars of sha256(pub) — short
// enough for a one-line stderr message, long enough to be unambiguous
// at the scale of one key per registered user.
func pubFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:12]
}
