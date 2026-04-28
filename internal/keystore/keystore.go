// Package keystore persists the verifier's Ed25519 keypair on disk so the
// same public key can be registered with the server once and reused on
// every subsequent `runlog-verifier verify` run.
//
// The path is fixed at ~/.runlog/key (configurable via RUNLOG_KEY_PATH for
// tests). The file is plain JSON with mode 0600 inside a 0700 directory,
// matching the convention used by other CLI tools for long-lived secrets:
//
//	{"private_key_b64": "...", "public_key_b64": "...", "version": 1}
//
// Save refuses to overwrite an existing file unless force=true so an
// accidental `runlog-verifier register` cannot clobber a registered key
// that the server (and any prior signed bundles) still trust.
package keystore

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotFound is returned by Load when the key file does not exist.
// Callers branch on this to print a "run register first" hint instead of
// the raw os error.
var ErrNotFound = errors.New("keystore: key file not found")

// ErrAlreadyExists is returned by Save when the destination file already
// exists and force=false. Mirrors os.ErrExist semantics but with a
// keystore-specific identity so callers can match without coupling to
// the underlying filesystem error.
var ErrAlreadyExists = errors.New("keystore: key file already exists")

// fileFormatVersion is written into every saved file; older formats can
// be migrated on Load if we ever bump it. Treat as load-bearing — bumping
// requires migration code.
const fileFormatVersion = 1

// onDiskFormat is what we marshal/unmarshal. Field names match the
// register-pubkey wire contract for the public_key_b64 field.
type onDiskFormat struct {
	PrivateKeyB64 string `json:"private_key_b64"`
	PublicKeyB64  string `json:"public_key_b64"`
	Version       int    `json:"version"`
}

// Path returns the resolved keystore path. Honors RUNLOG_KEY_PATH for
// tests; otherwise ~/.runlog/key. Returns an error only if the user's
// home directory cannot be determined and no override is set.
func Path() (string, error) {
	if override := os.Getenv("RUNLOG_KEY_PATH"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("keystore: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".runlog", "key"), nil
}

// Generate creates a fresh Ed25519 keypair via crypto/rand. It does not
// touch disk; pair Generate with Save to persist.
func Generate() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("keystore: ed25519.GenerateKey: %w", err)
	}
	return priv, pub, nil
}

// Load reads the key file at Path() and returns the parsed keypair.
// Returns ErrNotFound (wrapped) if the file is missing — callers branch
// on errors.Is(err, ErrNotFound) to distinguish "first run" from "corrupt
// file".
func Load() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	path, err := Path()
	if err != nil {
		return nil, nil, err
	}
	return loadFrom(path)
}

func loadFrom(path string) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w at %s", ErrNotFound, path)
		}
		return nil, nil, fmt.Errorf("keystore: read %s: %w", path, err)
	}

	var f onDiskFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("keystore: parse %s: %w", path, err)
	}
	if f.Version != fileFormatVersion {
		return nil, nil, fmt.Errorf(
			"keystore: %s has unsupported version %d (expected %d)",
			path, f.Version, fileFormatVersion,
		)
	}

	priv, err := base64.StdEncoding.DecodeString(f.PrivateKeyB64)
	if err != nil {
		return nil, nil, fmt.Errorf("keystore: decode private_key_b64: %w", err)
	}
	pub, err := base64.StdEncoding.DecodeString(f.PublicKeyB64)
	if err != nil {
		return nil, nil, fmt.Errorf("keystore: decode public_key_b64: %w", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf(
			"keystore: private key length %d, expected %d",
			len(priv), ed25519.PrivateKeySize,
		)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, nil, fmt.Errorf(
			"keystore: public key length %d, expected %d",
			len(pub), ed25519.PublicKeySize,
		)
	}

	return ed25519.PrivateKey(priv), ed25519.PublicKey(pub), nil
}

// Save persists the keypair to Path(). Creates the parent directory with
// mode 0700 if missing. Refuses to overwrite an existing file unless
// force=true (returns ErrAlreadyExists).
//
// The file is written via O_EXCL when force=false so two concurrent
// register runs can't both win and disagree about which key is registered.
func Save(priv ed25519.PrivateKey, pub ed25519.PublicKey, force bool) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf(
			"keystore: private key length %d, expected %d",
			len(priv), ed25519.PrivateKeySize,
		)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf(
			"keystore: public key length %d, expected %d",
			len(pub), ed25519.PublicKeySize,
		)
	}

	path, err := Path()
	if err != nil {
		return err
	}
	return saveTo(path, priv, pub, force)
}

func saveTo(path string, priv ed25519.PrivateKey, pub ed25519.PublicKey, force bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("keystore: mkdir %s: %w", dir, err)
	}
	// MkdirAll is a no-op when the dir exists, leaving its mode untouched —
	// tighten it explicitly so a previously-loose ~/.runlog/ gets fixed
	// the first time we save into it.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("keystore: chmod %s: %w", dir, err)
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w at %s (use --force to overwrite)", ErrAlreadyExists, path)
		}
		return fmt.Errorf("keystore: open %s: %w", path, err)
	}
	defer f.Close()

	body := onDiskFormat{
		PrivateKeyB64: base64.StdEncoding.EncodeToString(priv),
		PublicKeyB64:  base64.StdEncoding.EncodeToString(pub),
		Version:       fileFormatVersion,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("keystore: marshal: %w", err)
	}
	if _, err := f.Write(encoded); err != nil {
		return fmt.Errorf("keystore: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("keystore: sync %s: %w", path, err)
	}
	return nil
}
