// Command gen-bundle-vector emits one deterministic Ed25519-signed bundle
// vector for cross-language canonicalization tests.
//
// The output JSON manifest is consumed by the Python frozen-vector test
// fixture in the sibling runlog/ repo (server/tests/fixtures/). Re-run this
// tool whenever the Go canonicalizer in internal/sign changes (encoding/json
// defaults, omitempty rules, key ordering, struct field order) to refresh
// the frozen vector.
//
// Usage:
//
//	go run ./cmd/gen-bundle-vector            # JSON to stdout
//	go run ./cmd/gen-bundle-vector -out PATH  # JSON to file (parent dir created)
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/runlog-org/runlog-verifier/internal/sign"
)

// fixedSeed is a deterministic ed25519 seed — NOT a real signing key.
// Changing these bytes invalidates the frozen vector consumed by
// runlog/server/tests/fixtures/. The ASCII pattern below makes the
// test-only intent obvious to any future reader scanning for secrets.
var fixedSeed = [32]byte{
	0x72, 0x75, 0x6e, 0x6c, 0x6f, 0x67, 0x2d, 0x76, // "runlog-v"
	0x65, 0x63, 0x74, 0x6f, 0x72, 0x2d, 0x73, 0x65, // "ector-se"
	0x65, 0x64, 0x2d, 0x76, 0x30, 0x30, 0x31, 0x2e, // "ed-v001."
	0x66, 0x69, 0x78, 0x65, 0x64, 0x2e, 0x2e, 0x2e, // "fixed..."
}

// fixtureBundle is the load-bearing test value. Changing any field, value,
// or order here breaks the cross-language frozen-vector test. It exercises
// every canonicalizer match-point flagged in T62: struct field order,
// omitempty Tier/Reasons present, multi-key Fingerprint map (forces the
// encoding/json key-sort path), and BundleReason field order.
func fixtureBundle() sign.Bundle {
	return sign.Bundle{
		UnitID: "runlog-vector-001",
		Status: "verified",
		Tier:   "unit",
		Reasons: []sign.BundleReason{
			{Code: "tests-pass", Message: "all unit tests passed"},
			{Code: "fingerprint-match", Message: "execution fingerprint matched declared environment"},
		},
		Fingerprint: map[string]string{
			"go":   "1.22",
			"os":   "linux",
			"tier": "unit",
		},
	}
}

// generateVector builds the fixed bundle, signs it with the deterministic
// keypair, self-verifies, and returns the canonical bytes plus the base64
// signature and public key. Shared between main and the determinism test.
func generateVector() (canonical []byte, sigB64, pubB64 string, err error) {
	priv := ed25519.NewKeyFromSeed(fixedSeed[:])
	bundle := fixtureBundle()

	canonical, err = json.Marshal(bundle)
	if err != nil {
		return nil, "", "", fmt.Errorf("marshal bundle: %w", err)
	}

	signed, err := sign.Sign(bundle, priv)
	if err != nil {
		return nil, "", "", fmt.Errorf("sign bundle: %w", err)
	}

	ok, err := sign.Verify(signed)
	if err != nil {
		return nil, "", "", fmt.Errorf("self-verify: %w", err)
	}
	if !ok {
		return nil, "", "", fmt.Errorf("self-verify: signature did not validate")
	}

	return canonical, signed.Signature, signed.PublicKey, nil
}

type manifest struct {
	Meta               manifestMeta `json:"_meta"`
	BundleCanonicalB64 string       `json:"bundle_canonical_b64"`
	SignatureB64       string       `json:"signature_b64"`
	PublicKeyB64       string       `json:"public_key_b64"`
}

type manifestMeta struct {
	Generator string `json:"generator"`
	SeedHex   string `json:"seed_hex"`
	Comment   string `json:"comment"`
}

func run(out string) error {
	canonical, sigB64, pubB64, err := generateVector()
	if err != nil {
		return err
	}

	m := manifest{
		Meta: manifestMeta{
			Generator: "runlog-verifier/cmd/gen-bundle-vector",
			SeedHex:   hex.EncodeToString(fixedSeed[:]),
			Comment: "Frozen Ed25519-signed bundle vector for cross-language " +
				"canonicalization tests. Consumed by runlog/server/tests/fixtures/. " +
				"Regenerate by re-running this command after any change to " +
				"internal/sign canonicalization.",
		},
		BundleCanonicalB64: base64.StdEncoding.EncodeToString(canonical),
		SignatureB64:       sigB64,
		PublicKeyB64:       pubB64,
	}

	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	payload = append(payload, '\n')

	if out == "" {
		if _, err := os.Stdout.Write(payload); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		return nil
	}

	dir := filepath.Dir(out)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create parent dir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(out, payload, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", out, err)
	}
	return nil
}

func main() {
	fs := flag.NewFlagSet("gen-bundle-vector", flag.ExitOnError)
	out := fs.String("out", "", "write manifest to this path instead of stdout")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: gen-bundle-vector [-out PATH]\n\n")
		fmt.Fprintf(fs.Output(),
			"Emits a deterministic Ed25519-signed bundle vector as JSON.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "gen-bundle-vector: unexpected positional args: %v\n", fs.Args())
		fs.Usage()
		os.Exit(2)
	}

	if err := run(*out); err != nil {
		fmt.Fprintf(os.Stderr, "gen-bundle-vector: %v\n", err)
		os.Exit(1)
	}
}
