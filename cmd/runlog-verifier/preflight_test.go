package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// genTestPub returns a fresh Ed25519 public key for use as a test fixture.
func genTestPub(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub
}

// withPreflightServer spins up an httptest.NewServer with the given handler,
// wires it into preflightHTTPClient, and registers cleanup. Returns the
// server URL.
func withPreflightServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	preflightHTTPClient = srv.Client()
	t.Cleanup(func() {
		preflightHTTPClient = nil
		srv.Close()
	})
	return srv.URL
}

func TestCheckServerPubkey_200_Match(t *testing.T) {
	pub := genTestPub(t)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/pubkey" {
			t.Errorf("expected /v1/pubkey, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"public_key_b64":"` + pubB64 + `","key_id":"abc"}`))
	})

	if err := checkServerPubkey(url, "sk-test", pub); err != nil {
		t.Fatalf("expected nil error on match, got: %v", err)
	}
}

func TestCheckServerPubkey_200_Mismatch(t *testing.T) {
	localPub := genTestPub(t)
	otherPub := genTestPub(t)
	otherB64 := base64.StdEncoding.EncodeToString(otherPub)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"public_key_b64":"` + otherB64 + `","key_id":"abc"}`))
	})

	err := checkServerPubkey(url, "sk-test", localPub)
	if err == nil {
		t.Fatal("expected error on mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "different pubkey on file") {
		t.Errorf("error should mention different pubkey on file: %q", err.Error())
	}
}

func TestCheckServerPubkey_404_NotRegistered(t *testing.T) {
	pub := genTestPub(t)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"pubkey_not_registered","message":"no pubkey on file for this API key"}}`))
	})

	err := checkServerPubkey(url, "sk-test", pub)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "pubkey not registered") {
		t.Errorf("error should mention pubkey not registered: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "runlog-verifier register --email") {
		t.Errorf("error should mention register command: %q", err.Error())
	}
}

func TestCheckServerPubkey_401(t *testing.T) {
	pub := genTestPub(t)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"auth.unknown_key","message":"unknown API key"}}`))
	})

	err := checkServerPubkey(url, "sk-bogus", pub)
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "API key rejected") {
		t.Errorf("error should mention API key rejected: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "auth.unknown_key") {
		t.Errorf("error should surface server error type: %q", err.Error())
	}
}

func TestCheckServerPubkey_429_WithRetryAfter(t *testing.T) {
	pub := genTestPub(t)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down","retry_after_seconds":12}}`))
	})

	err := checkServerPubkey(url, "sk-test", pub)
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if !strings.Contains(err.Error(), "rate-limited") {
		t.Errorf("error should mention rate-limited: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "12") {
		t.Errorf("error should include the retry-after seconds: %q", err.Error())
	}
}

func TestCheckServerPubkey_429_WithoutRetryAfter(t *testing.T) {
	pub := genTestPub(t)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))
	})

	err := checkServerPubkey(url, "sk-test", pub)
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if !strings.Contains(err.Error(), "rate-limited") {
		t.Errorf("error should mention rate-limited: %q", err.Error())
	}
}

func TestCheckServerPubkey_503(t *testing.T) {
	pub := genTestPub(t)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"internal","message":"upstream broke"}}`))
	})

	err := checkServerPubkey(url, "sk-test", pub)
	if err == nil {
		t.Fatal("expected error on 503, got nil")
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Errorf("error should mention temporarily unavailable: %q", err.Error())
	}
}

func TestCheckServerPubkey_UnexpectedStatus(t *testing.T) {
	pub := genTestPub(t)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 418
	})

	err := checkServerPubkey(url, "sk-test", pub)
	if err == nil {
		t.Fatal("expected error on 418, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected response status 418") {
		t.Errorf("error should mention unexpected status 418: %q", err.Error())
	}
}

func TestCheckServerPubkey_NetworkError(t *testing.T) {
	pub := genTestPub(t)

	// Bind a port and immediately close it — connecting will fail without
	// the OS having to time out.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if err := checkServerPubkey("http://"+addr, "sk-test", pub); err == nil {
		t.Fatal("expected non-nil error for closed/unreachable server, got nil")
	}
}

func TestCheckServerPubkey_MalformedBody_200(t *testing.T) {
	pub := genTestPub(t)

	url := withPreflightServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not json`))
	})

	err := checkServerPubkey(url, "sk-test", pub)
	if err == nil {
		t.Fatal("expected error on malformed body, got nil")
	}
}

func TestResolvePreflightServer(t *testing.T) {
	tests := []struct {
		name     string
		override string
		env      string
		want     string
	}{
		{
			name:     "explicit override wins",
			override: "https://override.example.com",
			env:      "https://env.example.com",
			want:     "https://override.example.com",
		},
		{
			name:     "env fallback when no override",
			override: "",
			env:      "https://env.example.com",
			want:     "https://env.example.com",
		},
		{
			name:     "default when both empty",
			override: "",
			env:      "",
			want:     defaultRegisterServer,
		},
		{
			name:     "trailing slash trimmed from override",
			override: "https://override.example.com/",
			env:      "",
			want:     "https://override.example.com",
		},
		{
			name:     "trailing slash trimmed from env",
			override: "",
			env:      "https://env.example.com/",
			want:     "https://env.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RUNLOG_API_URL", tt.env)
			got := resolveServerURL(tt.override)
			if got != tt.want {
				t.Errorf("resolveServerURL(%q) with env=%q = %q, want %q",
					tt.override, tt.env, got, tt.want)
			}
		})
	}
}
