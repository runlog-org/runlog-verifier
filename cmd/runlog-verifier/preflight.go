package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const pubkeyGetPath = "/v1/pubkey"

// preflightHTTPClient is overridable in tests; nil means use a fresh
// http.Client with the registerHTTPTimeout (shared with register.go).
var preflightHTTPClient *http.Client

// resolvePreflightServer returns the server base URL using the same
// cascade as the register subcommand: explicit override > $RUNLOG_API_URL
// > defaultRegisterServer. Returned value has any trailing slash trimmed.
func resolvePreflightServer(override string) string {
	server := override
	if server == "" {
		if env := os.Getenv("RUNLOG_API_URL"); env != "" {
			server = env
		} else {
			server = defaultRegisterServer
		}
	}
	return strings.TrimRight(server, "/")
}

// checkServerPubkey issues GET /v1/pubkey and returns nil if the server's
// registered pubkey matches localPub. Returns a non-nil error with a
// user-actionable message otherwise — caller prints it to stderr and
// exits non-zero. The message is the verbatim string the user sees;
// do not wrap it.
//
// Reuses validateServerURL, serverErrorType, and the HTTP-client pattern
// from register.go. Same registerHTTPTimeout (10s) and maxResponseBytes
// cap as the POST.
func checkServerPubkey(server, apiKey string, localPub ed25519.PublicKey) error {
	server = strings.TrimRight(server, "/")
	endpoint, err := url.Parse(server + pubkeyGetPath)
	if err != nil {
		return fmt.Errorf("invalid server URL %q: %v", server, err)
	}
	if err := validateServerURL(endpoint); err != nil {
		return fmt.Errorf("server URL %q rejected: %v", server, err)
	}

	req, err := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("build pre-flight request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	client := preflightHTTPClient
	if client == nil {
		client = &http.Client{Timeout: registerHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %v", server, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read pre-flight response body: %v", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var ok struct {
			PublicKeyB64 string `json:"public_key_b64"`
			KeyID        string `json:"key_id"`
		}
		if err := json.Unmarshal(body, &ok); err != nil {
			return fmt.Errorf("server returned 200 but body was not parseable: %v", err)
		}
		serverPub, err := base64.StdEncoding.DecodeString(ok.PublicKeyB64)
		if err != nil {
			return fmt.Errorf("server returned 200 but public_key_b64 was not valid base64: %v", err)
		}
		if !bytes.Equal(serverPub, localPub) {
			return errors.New("server has a different pubkey on file than your local key — re-run 'runlog-verifier register --email <you>' to upload your current pubkey, or rotate the key")
		}
		return nil

	case http.StatusNotFound:
		// Don't surface the server's literal message since we craft our own
		// actionable one; the server message is essentially the same content
		// but we want a stable, testable string.
		_, _ = serverErrorType(body)
		return errors.New("pubkey not registered on the server — run 'runlog-verifier register --email <you>' first")

	case http.StatusUnauthorized:
		errType, _ := serverErrorType(body)
		if errType == "" {
			errType = "unknown"
		}
		return fmt.Errorf("API key rejected by server (%s) — check RUNLOG_API_KEY", errType)

	case http.StatusTooManyRequests:
		var withRetry struct {
			Error struct {
				RetryAfterSeconds int `json:"retry_after_seconds"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &withRetry)
		if withRetry.Error.RetryAfterSeconds > 0 {
			return fmt.Errorf("rate-limited by server, retry in %ds", withRetry.Error.RetryAfterSeconds)
		}
		return errors.New("rate-limited by server, retry shortly")

	case http.StatusServiceUnavailable:
		return errors.New("server temporarily unavailable, retry shortly")

	default:
		return fmt.Errorf("unexpected response status %d from server", resp.StatusCode)
	}
}
