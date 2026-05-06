// Package fingerprint captures the OS / runtime / git environment of the
// submitter's machine for inclusion in the signed bundle.
//
// The captured fingerprint is opaque to the platform — it doesn't gate
// verification. Phase 3's correlation engine reads it to attribute
// delayed failures to environment fingerprints (e.g., "kb:4821 fails on
// glibc < 2.36 but passes everywhere else").
package fingerprint

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// gitTimeout caps how long either `git rev-parse` or `git status --porcelain`
// is allowed to run during fingerprint capture. A hung git invocation
// (filesystem stall, repo lock contention) would otherwise block the entire
// `verify` run indefinitely; the fingerprint is best-effort, so missing it on
// a stuck host is preferable to deadlocking the verifier. 30s is loose enough
// to tolerate slow networked filesystems while still bounding worst-case
// blocking.
const gitTimeout = 30 * time.Second

// Print holds the captured environment snapshot. All fields are
// best-effort — the program does not fail if git is unavailable.
//
// Git state has three distinct cases:
//   - git binary not found OR working directory is not a git repo:
//     GitAvailable=false, GitCommit="", GitDirty=false.
//   - git found and inside a repo:
//     GitAvailable=true, GitCommit=<short SHA>, GitDirty=<true|false>.
type Print struct {
	OS           string `json:"os"`            // runtime.GOOS
	Arch         string `json:"arch"`          // runtime.GOARCH
	GoVersion    string `json:"go_version"`    // runtime.Version()
	GitAvailable bool   `json:"git_available"` // false if git absent or not a repo
	GitCommit    string `json:"git_commit"`    // short SHA; "" when GitAvailable=false
	GitDirty     bool   `json:"git_dirty"`     // false when GitAvailable=false
	CapturedAt   string `json:"captured_at"`   // RFC 3339, UTC
}

// captureGit is the path used to locate git. Overrideable in tests.
var captureGit = func() (string, error) { return exec.LookPath("git") }

// AsMap renders the fingerprint as the map[string]string the signed bundle
// expects. Booleans are stringified to "true"/"false" so the value type stays
// uniform across all keys — Bundle.Fingerprint is map[string]string so the
// signature canonicalisation doesn't need a heterogeneous-value pass. Callers
// in cmd/ that want to embed the fingerprint into a sign.Bundle should pass
// AsMap() directly.
func (p Print) AsMap() map[string]string {
	return map[string]string{
		"os":            p.OS,
		"arch":          p.Arch,
		"go_version":    p.GoVersion,
		"git_commit":    p.GitCommit,
		"captured_at":   p.CapturedAt,
		"git_available": boolStr(p.GitAvailable),
		"git_dirty":     boolStr(p.GitDirty),
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Capture collects the current environment. Git fields are populated
// via exec("git ..."); if git is not installed or the working directory
// is not a repository, GitAvailable is set to false and the remaining
// git fields are left at their zero values.
func Capture() Print {
	p := Print{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		GoVersion:  runtime.Version(),
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
	}

	gitPath, err := captureGit()
	if err != nil {
		// git binary not found — GitAvailable stays false.
		return p
	}

	// Resolve HEAD commit short SHA. Failure (or timeout) means we're not in
	// a usable repo; treat both as GitAvailable=false so a hung git doesn't
	// block the verifier.
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	out, err := exec.CommandContext(ctx, gitPath, "rev-parse", "--short", "HEAD").Output()
	cancel()
	if err != nil {
		// Not a git working directory (or git hung past gitTimeout) —
		// GitAvailable stays false.
		return p
	}
	p.GitAvailable = true
	p.GitCommit = strings.TrimSpace(string(out))

	// Detect uncommitted changes (only reachable when we're in a repo).
	ctx, cancel = context.WithTimeout(context.Background(), gitTimeout)
	if out, err := exec.CommandContext(ctx, gitPath, "status", "--porcelain").Output(); err == nil {
		p.GitDirty = strings.TrimSpace(string(out)) != ""
	}
	cancel()

	return p
}
