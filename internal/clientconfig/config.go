// Package clientconfig persists the verifier's API credentials on disk so
// the cohesive `register --email` flow can hand the issued key off to
// subsequent `verify` runs without forcing the user to manually
// `export RUNLOG_API_KEY`.
//
// The path is fixed at ~/.runlog/config.json (configurable via
// RUNLOG_CONFIG_PATH for tests). The file is plain JSON with mode 0600
// inside a 0700 directory — same convention as the keystore module:
//
//	{"version": 1, "api_key": "...", "api_key_id": "...", "registered_at": "..."}
//
// Save refuses to overwrite an existing file when the api_key_id differs
// unless force=true so a second `register` against a different account
// can't silently shadow the first.
package clientconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNotFound is returned by Load when the config file does not exist.
// Callers branch on errors.Is(err, ErrNotFound) to distinguish "no
// config persisted yet" from a corrupt file.
var ErrNotFound = errors.New("clientconfig: config file not found")

// ErrAlreadyExists is returned by Save when the destination already exists
// with a different api_key_id and force=false. Distinguishing this from a
// generic os.ErrExist lets callers print the user-actionable "rerun with
// --force" hint without coupling to filesystem semantics.
var ErrAlreadyExists = errors.New("clientconfig: config file already exists with a different api_key_id")

// fileFormatVersion is written into every saved file; bumping requires a
// migration pass on Load.
const fileFormatVersion = 1

// onDiskFormat is the JSON shape persisted to ~/.runlog/config.json.
type onDiskFormat struct {
	Version      int    `json:"version"`
	APIKey       string `json:"api_key"`
	APIKeyID     string `json:"api_key_id"`
	RegisteredAt string `json:"registered_at"`
}

// Path returns the resolved config path. Honors RUNLOG_CONFIG_PATH for
// tests; otherwise ~/.runlog/config.json. Returns an error only if the
// user's home directory cannot be determined and no override is set.
func Path() (string, error) {
	if override := os.Getenv("RUNLOG_CONFIG_PATH"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("clientconfig: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".runlog", "config.json"), nil
}

// Load reads the config at Path() and returns (api_key, api_key_id, err).
// Returns ErrNotFound (wrapped) if the file is missing.
func Load() (string, string, error) {
	path, err := Path()
	if err != nil {
		return "", "", err
	}
	return loadFrom(path)
}

func loadFrom(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("%w at %s", ErrNotFound, path)
		}
		return "", "", fmt.Errorf("clientconfig: read %s: %w", path, err)
	}

	var f onDiskFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return "", "", fmt.Errorf("clientconfig: parse %s: %w", path, err)
	}
	if f.Version != fileFormatVersion {
		return "", "", fmt.Errorf(
			"clientconfig: %s has unsupported version %d (expected %d)",
			path, f.Version, fileFormatVersion,
		)
	}
	if f.APIKey == "" {
		return "", "", fmt.Errorf("clientconfig: %s missing api_key", path)
	}
	if f.APIKeyID == "" {
		return "", "", fmt.Errorf("clientconfig: %s missing api_key_id", path)
	}
	return f.APIKey, f.APIKeyID, nil
}

// Save writes (apiKey, apiKeyID) to Path(). Creates the parent directory
// with mode 0700 if missing. If a config already exists with a DIFFERENT
// api_key_id and force=false, returns ErrAlreadyExists. If the existing
// config has the SAME api_key_id (e.g. re-running register on the same
// account), Save is a no-op write that refreshes registered_at.
func Save(apiKey, apiKeyID string, force bool) error {
	if apiKey == "" {
		return errors.New("clientconfig: api_key is required")
	}
	if apiKeyID == "" {
		return errors.New("clientconfig: api_key_id is required")
	}
	path, err := Path()
	if err != nil {
		return err
	}
	return saveTo(path, apiKey, apiKeyID, force)
}

func saveTo(path, apiKey, apiKeyID string, force bool) error {
	// Conflict detection: if a config already exists with a different
	// api_key_id, refuse unless force. We deliberately read+inspect rather
	// than relying on O_EXCL — a same-key_id re-register is an idempotent
	// refresh, not a conflict.
	if existing, err := os.ReadFile(path); err == nil {
		var f onDiskFormat
		if jsonErr := json.Unmarshal(existing, &f); jsonErr == nil {
			if f.APIKeyID != "" && f.APIKeyID != apiKeyID && !force {
				return fmt.Errorf(
					"%w at %s (existing api_key_id=%s, new=%s; use --force to overwrite)",
					ErrAlreadyExists, path, f.APIKeyID, apiKeyID,
				)
			}
		}
		// If the existing file is corrupt, fall through and overwrite —
		// there's no usable api_key_id to preserve.
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clientconfig: stat %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("clientconfig: mkdir %s: %w", dir, err)
	}
	// MkdirAll is a no-op when the dir exists, leaving its mode untouched —
	// tighten it explicitly so a previously-loose ~/.runlog/ gets fixed.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("clientconfig: chmod %s: %w", dir, err)
	}

	body := onDiskFormat{
		Version:      fileFormatVersion,
		APIKey:       apiKey,
		APIKeyID:     apiKeyID,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}
	encoded, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Errorf("clientconfig: marshal: %w", err)
	}
	// O_TRUNC is intentional — we've already enforced the conflict policy
	// above. WriteFile applies 0o600 on creation; on overwrite the existing
	// permissions are preserved by the kernel, but if a future change ever
	// loosened them, an explicit Chmod would be safer.
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("clientconfig: write %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("clientconfig: chmod %s: %w", path, err)
	}
	return nil
}
