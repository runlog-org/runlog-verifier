package keystore

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// withKeyPath installs a temp keystore path for the duration of t. We
// route every test through RUNLOG_KEY_PATH so they never touch the real
// ~/.runlog/.
func withKeyPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "runlog", "key")
	t.Setenv("RUNLOG_KEY_PATH", path)
	return path
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withKeyPath(t)

	priv, pub, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(priv, pub, false); err != nil {
		t.Fatalf("Save: %v", err)
	}

	priv2, pub2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !priv.Equal(priv2) {
		t.Errorf("private key mismatch after round trip")
	}
	if !pub.Equal(pub2) {
		t.Errorf("public key mismatch after round trip")
	}
}

func TestLoadReturnsNotFoundWhenMissing(t *testing.T) {
	withKeyPath(t)

	_, _, err := Load()
	if err == nil {
		t.Fatalf("expected error when key file absent, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSaveRefusesOverwriteWithoutForce(t *testing.T) {
	withKeyPath(t)

	priv1, pub1, _ := Generate()
	if err := Save(priv1, pub1, false); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	priv2, pub2, _ := Generate()
	err := Save(priv2, pub2, false)
	if err == nil {
		t.Fatalf("expected ErrAlreadyExists on second Save without force, got nil")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}

	// Existing file is untouched.
	got, _, err := Load()
	if err != nil {
		t.Fatalf("Load after refused overwrite: %v", err)
	}
	if !got.Equal(priv1) {
		t.Fatalf("file was clobbered despite refused overwrite")
	}
}

func TestSaveOverwritesWithForce(t *testing.T) {
	withKeyPath(t)

	priv1, pub1, _ := Generate()
	if err := Save(priv1, pub1, false); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	priv2, pub2, _ := Generate()
	if err := Save(priv2, pub2, true); err != nil {
		t.Fatalf("second Save with force: %v", err)
	}
	got, _, err := Load()
	if err != nil {
		t.Fatalf("Load after force: %v", err)
	}
	if !got.Equal(priv2) {
		t.Fatalf("force=true did not overwrite")
	}
}

func TestSaveCreatesDirectoryMode0700(t *testing.T) {
	path := withKeyPath(t)

	priv, pub, _ := Generate()
	if err := Save(priv, pub, false); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %o, want 0700", mode)
	}

	finfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if mode := finfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
}

func TestLoadOnMalformedFile(t *testing.T) {
	path := withKeyPath(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatalf("expected error on malformed file")
	}
}

func TestLoadOnTruncatedFile(t *testing.T) {
	path := withKeyPath(t)

	priv, pub, _ := Generate()
	if err := Save(priv, pub, false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Truncate to half-length so json may or may not parse, but if it
	// does the bytes won't be valid keys. Either way: error, not panic.
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatalf("rewrite truncated: %v", err)
	}

	if _, _, err := Load(); err == nil {
		t.Fatalf("expected error on truncated file")
	}
}

func TestLoadOnWrongVersion(t *testing.T) {
	path := withKeyPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := onDiskFormat{
		PrivateKeyB64: "AA==",
		PublicKeyB64:  "AA==",
		Version:       99,
	}
	encoded, _ := json.Marshal(body)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := Load(); err == nil {
		t.Fatalf("expected version mismatch error")
	}
}

func TestLoadKeysAreUsable(t *testing.T) {
	withKeyPath(t)

	priv, pub, _ := Generate()
	if err := Save(priv, pub, false); err != nil {
		t.Fatalf("Save: %v", err)
	}

	priv2, pub2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	msg := []byte("smoke")
	sig := ed25519.Sign(priv2, msg)
	if !ed25519.Verify(pub2, msg, sig) {
		t.Fatalf("loaded keypair does not sign+verify a roundtrip")
	}
}
