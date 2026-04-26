// Package sign implements Ed25519 signing of a canonical-JSON bundle.
//
// For v0.1 the keypair is generated fresh on each verify run (the binary
// doesn't ship with embedded keys yet — that's a reproducible-build
// concern that lands once we have CI). The signature is real; only the
// key provenance is stubbed.
//
// Canonicalization: Go's encoding/json marshals struct fields in declaration
// order (not alphabetical). Because Bundle is a concrete struct — not a
// map — re-marshalling it is deterministic: field order is fixed by the
// struct definition above and does not change between runs. This is the
// canonical form used for signing. We document it here rather than
// introducing a key-sort pass that would add complexity without correctness
// benefit for a typed struct.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Bundle is the payload that gets signed. Fields are in declaration order;
// that order is the canonical serialization — do not reorder without bumping
// the bundle format version.
type Bundle struct {
	UnitID       string            `json:"unit_id"`
	Status       string            `json:"status"`
	Tier         string            `json:"tier,omitempty"`
	Reasons      []BundleReason    `json:"reasons,omitempty"`
	Fingerprint  map[string]string `json:"fingerprint"`
	// Future: TestResults, MutationResults, CassettePaths
}

// BundleReason mirrors the verify.Reason struct so the sign package does
// not depend on the verify package (avoids an import cycle if the verify
// package later wants to compose Bundles directly).
type BundleReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Signed wraps a Bundle with its Ed25519 signature and the corresponding
// public key, both base64-encoded (standard encoding, no padding stripped).
type Signed struct {
	Bundle    Bundle `json:"bundle"`
	Signature string `json:"signature"`  // base64-encoded Ed25519 signature
	PublicKey string `json:"public_key"` // base64-encoded Ed25519 public key
}

// GenerateKeypair creates a fresh Ed25519 keypair using crypto/rand.
// Returns (publicKey, privateKey, error). For dev/keygen use only — see
// the keygen subcommand. Production keys are embedded in reproducible-build
// releases (deferred to Phase 2 CI).
func GenerateKeypair() (publicKey []byte, privateKey []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ed25519.GenerateKey: %w", err)
	}
	return pub, priv, nil
}

// Sign serialises b to canonical JSON (struct-declaration field order) and
// signs the bytes with privateKey using Ed25519. Returns a Signed value
// containing the original bundle, the base64 signature, and the base64
// public key derived from privateKey.
func Sign(b Bundle, privateKey []byte) (Signed, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return Signed{}, fmt.Errorf("sign: private key must be %d bytes, got %d",
			ed25519.PrivateKeySize, len(privateKey))
	}

	canonical, err := json.Marshal(b)
	if err != nil {
		return Signed{}, fmt.Errorf("sign: marshal bundle: %w", err)
	}

	priv := ed25519.PrivateKey(privateKey)
	sig := ed25519.Sign(priv, canonical)
	pub := priv.Public().(ed25519.PublicKey)

	return Signed{
		Bundle:    b,
		Signature: base64.StdEncoding.EncodeToString(sig),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}, nil
}

// Verify re-derives the canonical bytes from s.Bundle, decodes the signature
// and public key, and runs ed25519.Verify. Returns (true, nil) on success.
func Verify(s Signed) (bool, error) {
	canonical, err := json.Marshal(s.Bundle)
	if err != nil {
		return false, fmt.Errorf("verify: marshal bundle: %w", err)
	}

	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return false, fmt.Errorf("verify: decode signature: %w", err)
	}

	pub, err := base64.StdEncoding.DecodeString(s.PublicKey)
	if err != nil {
		return false, fmt.Errorf("verify: decode public key: %w", err)
	}

	if len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("verify: public key must be %d bytes, got %d",
			ed25519.PublicKeySize, len(pub))
	}

	ok := ed25519.Verify(ed25519.PublicKey(pub), canonical, sig)
	return ok, nil
}
