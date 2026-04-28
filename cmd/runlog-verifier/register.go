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
	"os"
	"strings"
	"time"

	"github.com/runlog-org/runlog-verifier/internal/keystore"
)

const (
	defaultRegisterServer = "https://api.runlog.org"
	registerPath          = "/v1/register-pubkey"
	registerHTTPTimeout   = 10 * time.Second
)

// registerExitOK / registerExitUser / registerExitNet keep the exit-code
// mapping legible inside the handler. Aligned with the verifier's
// existing exit-code conventions documented in usage().
const (
	registerExitOK   = 0
	registerExitUser = 1 // missing env, 401/400/409, refusal to overwrite
	registerExitNet  = 2 // network error, unexpected status, internal failure
)

// registerHTTPClient is overridable in tests; nil means use a fresh
// http.Client with the registerHTTPTimeout.
var registerHTTPClient *http.Client

// runRegister implements the `register` subcommand. stdout/stderr are
// passed in (rather than hardcoded to os.Stdout/Stderr) so tests can
// capture output without juggling pipes around the real fds.
func runRegister(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		emailFlag  = fs.String("email", "", "email address to associate with this key (required)")
		forceFlag  = fs.Bool("force", false, "overwrite an existing local keypair at ~/.runlog/key")
		serverFlag = fs.String("server", "", "registration server base URL (default: $RUNLOG_API_URL or "+defaultRegisterServer+")")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: runlog-verifier register --email <addr> [--force] [--server <url>]")
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

	apiKey := os.Getenv("RUNLOG_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(stderr,
			"register: RUNLOG_API_KEY is not set. "+
				"Get one at https://runlog.org/register and export it before retrying.")
		return registerExitUser
	}

	server := *serverFlag
	if server == "" {
		if env := os.Getenv("RUNLOG_API_URL"); env != "" {
			server = env
		} else {
			server = defaultRegisterServer
		}
	}
	server = strings.TrimRight(server, "/")
	endpoint, err := url.Parse(server + registerPath)
	if err != nil {
		fmt.Fprintf(stderr, "register: invalid server URL %q: %v\n", server, err)
		return registerExitUser
	}

	// 1. Load or generate the keypair. We only upload the public key,
	// but loading the private key alongside validates the file is
	// well-formed before we make a network round-trip.
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

	// 2. POST to the server.
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	resp, body, err := postRegisterPubkey(endpoint.String(), apiKey, pubB64)
	if err != nil {
		fmt.Fprintf(stderr, "register: could not reach %s: %v\n", server, err)
		return registerExitNet
	}

	// 3. Map response. The server emits errors as
	//    {"error": {"type": "...", "message": "..."}} (nested object) on
	//    every non-2xx; success is {"status": "registered", "key_id": "..."}.
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
			"status":   "registered",
			"key_id":   ok.KeyID,
			"key_path": keyPath,
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
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
			"register: authentication failed: %s. Verify RUNLOG_API_KEY.\n",
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

	client := registerHTTPClient
	if client == nil {
		client = &http.Client{Timeout: registerHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
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

// pubFingerprint returns the first 12 hex chars of sha256(pub) — short
// enough for a one-line stderr message, long enough to be unambiguous
// at the scale of one key per registered user.
func pubFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:12]
}
