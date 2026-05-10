package clientconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// withConfigPath installs a temp config path for the duration of t. We
// route every test through RUNLOG_CONFIG_PATH so they never touch the
// real ~/.runlog/.
func withConfigPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "runlog", "config.json")
	t.Setenv("RUNLOG_CONFIG_PATH", path)
	return path
}

func TestPathHonorsEnvOverride(t *testing.T) {
	t.Setenv("RUNLOG_CONFIG_PATH", "/tmp/whatever/config.json")
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got != "/tmp/whatever/config.json" {
		t.Errorf("Path = %q, want override", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := withConfigPath(t)

	if err := Save("sk-runlog-aaaaaaaaaaaa-deadbeef", "aaaaaaaaaaaa", false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	key, id, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if key != "sk-runlog-aaaaaaaaaaaa-deadbeef" {
		t.Errorf("api_key = %q", key)
	}
	if id != "aaaaaaaaaaaa" {
		t.Errorf("api_key_id = %q", id)
	}

	// File mode is 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}

	// Parent dir is 0700.
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %o, want 0700", mode)
	}
}

func TestLoadReturnsNotFoundWhenMissing(t *testing.T) {
	withConfigPath(t)

	_, _, err := Load()
	if err == nil {
		t.Fatalf("expected error when config absent")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSaveRefusesDifferentKeyIDWithoutForce(t *testing.T) {
	withConfigPath(t)

	if err := Save("sk-1-deadbeef", "aaaaaaaaaaaa", false); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	err := Save("sk-2-deadbeef", "bbbbbbbbbbbb", false)
	if err == nil {
		t.Fatalf("expected ErrAlreadyExists, got nil")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}

	// Existing config untouched.
	key, id, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if key != "sk-1-deadbeef" || id != "aaaaaaaaaaaa" {
		t.Errorf("config was clobbered despite refused overwrite: key=%q id=%q", key, id)
	}
}

func TestSaveOverwritesDifferentKeyIDWithForce(t *testing.T) {
	withConfigPath(t)

	if err := Save("sk-1-deadbeef", "aaaaaaaaaaaa", false); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := Save("sk-2-deadbeef", "bbbbbbbbbbbb", true); err != nil {
		t.Fatalf("force Save: %v", err)
	}
	key, id, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if key != "sk-2-deadbeef" || id != "bbbbbbbbbbbb" {
		t.Errorf("force did not overwrite: key=%q id=%q", key, id)
	}
}

func TestSaveAllowsSameKeyIDIdempotent(t *testing.T) {
	withConfigPath(t)

	if err := Save("sk-1-deadbeef", "aaaaaaaaaaaa", false); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	// Re-saving with the same api_key_id should succeed even without
	// --force — registering twice on the same account is an idempotent
	// refresh, not a conflict.
	if err := Save("sk-1-newer", "aaaaaaaaaaaa", false); err != nil {
		t.Fatalf("idempotent re-Save: %v", err)
	}
	key, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if key != "sk-1-newer" {
		t.Errorf("idempotent re-save did not refresh api_key: %q", key)
	}
}

func TestSaveRequiresFields(t *testing.T) {
	withConfigPath(t)

	if err := Save("", "id", false); err == nil {
		t.Errorf("expected error on empty api_key")
	}
	if err := Save("key", "", false); err == nil {
		t.Errorf("expected error on empty api_key_id")
	}
}
