// Command runlog-verifier is the signed verification agent (Phase 2 stub).
//
// Today it accepts a runlog entry YAML, performs structural sanity checks,
// captures the host fingerprint, and signs a canonical-JSON bundle with
// an embedded Ed25519 keypair. The differential-execution and mutation-
// testing phases (docs/03-verification-and-provenance.md §5.3) are not
// implemented yet — those are the next Phase 2 deliverables.
//
// Usage:
//
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
	"flag"
	"fmt"
	"os"

	"github.com/runlog/verifier/internal/fingerprint"
	"github.com/runlog/verifier/internal/sign"
	"gopkg.in/yaml.v3"
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
	fmt.Fprintln(os.Stderr, `runlog-verifier — Runlog signed verification agent (Phase 2 stub)

Usage:
  runlog-verifier verify <entry.yaml>
        Read entry YAML, validate required fields, capture host fingerprint,
        sign a canonical-JSON bundle, and emit JSON to stdout.

  runlog-verifier keygen
        Generate a fresh Ed25519 keypair and emit JSON to stdout.
        DEV ONLY — production keys are embedded in reproducible-build releases.

  runlog-verifier version
  runlog-verifier --version
        Print version string and exit.

Exit codes: 0 success, 1 user error, 2 internal error.`)
}

func printVersion() {
	fmt.Printf("runlog-verifier %s (commit %s)\n", Version, Commit)
}

// entry is a minimal typed view of the YAML. We only decode the fields
// required for structural validation; the rest of the document is opaque.
type entry struct {
	UnitID          string      `yaml:"unit_id"`
	Domain          []string    `yaml:"domain"`
	FailedApproach  interface{} `yaml:"failed_approach"`
	WorkingApproach interface{} `yaml:"working_approach"`
}

// runVerify implements the `verify` subcommand.
// Returns an exit code (0, 1, or 2).
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
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: read %s: %v\n", path, err)
		return 1
	}

	var e entry
	if err := yaml.Unmarshal(data, &e); err != nil {
		fmt.Fprintf(os.Stderr, "verify: parse YAML %s: %v\n", path, err)
		return 1
	}

	// Structural validation: required top-level keys.
	var missing []string
	if e.UnitID == "" {
		missing = append(missing, "unit_id")
	}
	if len(e.Domain) == 0 {
		missing = append(missing, "domain")
	}
	if e.FailedApproach == nil {
		missing = append(missing, "failed_approach")
	}
	if e.WorkingApproach == nil {
		missing = append(missing, "working_approach")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "verify: entry is missing required fields: %v\n", missing)
		return 1
	}

	// Capture the host fingerprint.
	fp := fingerprint.Capture()
	fpMap := map[string]string{
		"os":          fp.OS,
		"arch":        fp.Arch,
		"go_version":  fp.GoVersion,
		"git_commit":  fp.GitCommit,
		"captured_at": fp.CapturedAt,
	}
	if fp.GitDirty {
		fpMap["git_dirty"] = "true"
	} else {
		fpMap["git_dirty"] = "false"
	}

	// Generate a per-run keypair (key embedding deferred to Phase 2 CI).
	_, priv, err := sign.GenerateKeypair()
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: keygen: %v\n", err)
		return 2
	}

	bundle := sign.Bundle{
		UnitID:      e.UnitID,
		Status:      "ok-stub",
		Fingerprint: fpMap,
	}

	signed, err := sign.Sign(bundle, priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: sign: %v\n", err)
		return 2
	}

	out := map[string]interface{}{
		"status":      signed.Bundle.Status,
		"unit_id":     signed.Bundle.UnitID,
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
	return 0
}

// runKeygen implements the `keygen` subcommand.
// DEV ONLY — production keys are embedded in reproducible-build releases.
// Returns an exit code.
func runKeygen(args []string) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: runlog-verifier keygen")
		fmt.Fprintln(os.Stderr, "       DEV ONLY — production keys are embedded in reproducible-build releases.")
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
		"note":            "DEV ONLY — production keys are embedded in reproducible-build releases.",
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "keygen: encode output: %v\n", err)
		return 2
	}
	return 0
}
