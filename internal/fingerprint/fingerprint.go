// Package fingerprint captures the OS / runtime / git environment of the
// submitter's machine for inclusion in the signed bundle.
//
// The captured fingerprint is opaque to the platform — it doesn't gate
// verification. Phase 3's correlation engine reads it to attribute
// delayed failures to environment fingerprints (e.g., "kb:4821 fails on
// glibc < 2.36 but passes everywhere else").
package fingerprint

import (
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Print holds the captured environment snapshot. All fields are
// best-effort — the program does not fail if git is unavailable.
type Print struct {
	OS         string `json:"os"`          // runtime.GOOS
	Arch       string `json:"arch"`        // runtime.GOARCH
	GoVersion  string `json:"go_version"`  // runtime.Version()
	GitCommit  string `json:"git_commit"`  // best-effort; "" on failure
	GitDirty   bool   `json:"git_dirty"`   // best-effort; false on failure
	CapturedAt string `json:"captured_at"` // RFC 3339, UTC
}

// Capture collects the current environment. Git fields are populated
// via exec("git ..."); if git is not installed or the working directory
// is not a repository, those fields are left at their zero values.
func Capture() Print {
	p := Print{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		GoVersion:  runtime.Version(),
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Best-effort: resolve HEAD commit short SHA.
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		p.GitCommit = strings.TrimSpace(string(out))
	}

	// Best-effort: detect uncommitted changes.
	if out, err := exec.Command("git", "status", "--porcelain").Output(); err == nil {
		p.GitDirty = strings.TrimSpace(string(out)) != ""
	}

	return p
}
