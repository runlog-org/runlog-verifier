// Command runlog-verifier is the signed verification agent.
//
// For an `assertion_only` entry the verifier runs the declarative checks
// from docs/03-verification-and-provenance.md §5.3 — branch presence,
// non-tautology, mutation structure, mutation discrimination, and
// primitives-allowlist — and signs a canonical-JSON bundle that records
// the outcome. `unit` and `integration` entries are accepted as
// well-formed but exit with status `tier_unsupported`; subprocess
// execution and cassette replay are still to land in Phase 2.
//
// Usage:
//
//	runlog-verifier register --email <addr>
//	runlog-verifier verify <entry.yaml>
//	runlog-verifier --version
//	runlog-verifier version
//	runlog-verifier keygen
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/runlog-org/runlog-verifier/internal/fingerprint"
	"github.com/runlog-org/runlog-verifier/internal/keystore"
	"github.com/runlog-org/runlog-verifier/internal/sign"
	"github.com/runlog-org/runlog-verifier/internal/verify"
)

// Version and Commit are injected at build time via -ldflags.
// They default to "dev" / "unknown" when the binary is built without the
// Makefile (e.g. `go run ./cmd/runlog-verifier`).
var (
	Version = "dev"
	Commit  = "unknown"
)

func main() {
	// Top-level --version flag (also handled as a subcommand below).
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Usage = usage
	flag.Parse()

	if *versionFlag {
		printVersion()
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	switch args[0] {
	case "register":
		os.Exit(runRegister(args[1:], os.Stdout, os.Stderr))
	case "verify":
		os.Exit(runVerify(args[1:]))
	case "keygen":
		os.Exit(runKeygen(args[1:]))
	case "version":
		printVersion()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "runlog-verifier: unknown subcommand %q\n\n", args[0])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `runlog-verifier — Runlog signed verification agent

Usage:
  runlog-verifier register --email <addr> [--force] [--server <url>]
        Generate (or load) a persistent Ed25519 keypair at ~/.runlog/key
        and register the public key with the Runlog server. Required once
        before `+"`verify`"+` can produce server-acceptable bundles.
        Reads RUNLOG_API_KEY from the environment; --server overrides
        RUNLOG_API_URL (default: https://api.runlog.org).

  runlog-verifier verify <entry.yaml>
        Run declarative verification on an assertion_only entry, capture the
        host fingerprint, sign a canonical-JSON bundle, and emit JSON to
        stdout. unit / integration tiers are accepted as well-formed but
        exit with status tier_unsupported until their runners land.

  runlog-verifier keygen
        Generate a fresh Ed25519 keypair and emit JSON to stdout.
        DEV ONLY — does NOT touch the persistent keystore at ~/.runlog/key.
        Use `+"`register`"+` for the production flow.

  runlog-verifier version
  runlog-verifier --version
        Print version string and exit.

Exit codes:
  0 verified, 1 user error, 2 internal error,
  3 rejected, 4 tier not yet implemented.`)
}

func printVersion() {
	fmt.Printf("runlog-verifier %s (commit %s)\n", Version, Commit)
}

// readEntryFile reads up to verify.MaxEntryBytes+1 bytes from path so we can
// detect — and reject — entries that exceed the cap without ever materialising
// an unbounded byte slice. yaml.Unmarshal of a multi-MiB file is a trivial
// memory-DoS vector that we don't need to support; entries are hand-authored
// and well under 1 MiB in practice.
func readEntryFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, verify.MaxEntryBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > verify.MaxEntryBytes {
		return nil, verify.ErrEntryTooLarge
	}
	return data, nil
}

// runVerify implements the `verify` subcommand.
// Returns an exit code:
//
//	0 — entry verified
//	1 — user error (bad args, unreadable file, malformed YAML, missing key)
//	2 — internal error (key load / signing failure)
//	3 — entry rejected (one or more declarative checks failed)
//	4 — verification tier not yet implemented in this build
func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: runlog-verifier verify <entry.yaml>")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "verify: expected exactly one argument: <entry.yaml>")
		fs.Usage()
		return 1
	}

	path := fs.Arg(0)
	data, err := readEntryFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: read %s: %v\n", path, err)
		return 1
	}

	res, err := verify.Run(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		return 1
	}

	// Capture the host fingerprint regardless of outcome — the platform
	// uses it to attribute environment-correlated failures even on
	// rejected entries.
	fpMap := fingerprint.Capture().AsMap()

	// Load the persistent keypair registered with the server. Refuse to
	// fall back to an ephemeral key — the server-side signature check
	// would silently fail in a way that's hard to diagnose.
	priv, _, err := keystore.Load()
	if err != nil {
		if errors.Is(err, keystore.ErrNotFound) {
			fmt.Fprintln(os.Stderr,
				"verify: no registration key found. "+
					"Run 'runlog-verifier register --email <addr>' first.")
			return 1
		}
		fmt.Fprintf(os.Stderr, "verify: load key: %v\n", err)
		return 2
	}

	reasons := make([]sign.BundleReason, len(res.Reasons))
	for i, r := range res.Reasons {
		reasons[i] = sign.BundleReason{Code: r.Code, Message: r.Message}
	}

	bundle := sign.Bundle{
		UnitID:      res.UnitID,
		Status:      res.Status,
		Tier:        res.Tier,
		Reasons:     reasons,
		Fingerprint: fpMap,
	}

	signed, err := sign.Sign(bundle, priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: sign: %v\n", err)
		return 2
	}

	out := map[string]any{
		"status":      signed.Bundle.Status,
		"unit_id":     signed.Bundle.UnitID,
		"tier":        signed.Bundle.Tier,
		"reasons":     signed.Bundle.Reasons,
		"fingerprint": signed.Bundle.Fingerprint,
		"signature":   signed.Signature,
		"public_key":  signed.PublicKey,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "verify: encode output: %v\n", err)
		return 2
	}

	switch res.Status {
	case "verified":
		return 0
	case "tier_unsupported":
		return 4
	default:
		return 3
	}
}

// runKeygen implements the `keygen` subcommand.
// DEV ONLY — does NOT touch the persistent keystore. Production flow is
// `register`, which writes ~/.runlog/key and uploads the pubkey.
// Returns an exit code.
func runKeygen(args []string) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: runlog-verifier keygen")
		fmt.Fprintln(os.Stderr, "       DEV ONLY — does not touch ~/.runlog/key. Use `register` for production.")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keygen: %v\n", err)
		return 2
	}

	out := map[string]string{
		"public_key_b64":  base64.StdEncoding.EncodeToString(pub),
		"private_key_b64": base64.StdEncoding.EncodeToString(priv),
		"note":            "DEV ONLY — does not touch ~/.runlog/key. Use `register` for production.",
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "keygen: encode output: %v\n", err)
		return 2
	}
	return 0
}
