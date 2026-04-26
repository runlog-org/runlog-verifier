package fingerprint

import (
	"strings"
	"testing"
)

func TestCaptureNonEmpty(t *testing.T) {
	p := Capture()

	if p.OS == "" {
		t.Errorf("expected non-empty OS")
	}
	if p.Arch == "" {
		t.Errorf("expected non-empty Arch")
	}
	if p.GoVersion == "" {
		t.Errorf("expected non-empty GoVersion")
	}
	if p.CapturedAt == "" {
		t.Errorf("expected non-empty CapturedAt")
	}
}

// TestCaptureGitAvailableInRepo verifies that GitAvailable=true when the
// tests run inside this git-tracked repository (the normal CI / dev path).
func TestCaptureGitAvailableInRepo(t *testing.T) {
	p := Capture()
	if !p.GitAvailable {
		t.Skip("git not available or not in a git repo — skipping git-available assertion")
	}
	if p.GitCommit == "" {
		t.Errorf("GitAvailable=true but GitCommit is empty")
	}
}

// TestCaptureGitUnavailable exercises the GitAvailable=false path by
// substituting a resolver that pretends the git binary does not exist.
func TestCaptureGitUnavailable(t *testing.T) {
	orig := captureGit
	t.Cleanup(func() { captureGit = orig })

	captureGit = func() (string, error) {
		return "", &notFoundError{"git"}
	}

	p := Capture()
	if p.GitAvailable {
		t.Errorf("expected GitAvailable=false when git binary is absent")
	}
	if p.GitCommit != "" {
		t.Errorf("expected GitCommit=\"\" when GitAvailable=false, got %q", p.GitCommit)
	}
	if p.GitDirty {
		t.Errorf("expected GitDirty=false when GitAvailable=false")
	}
	// Core fields must still be populated.
	if p.OS == "" {
		t.Errorf("expected non-empty OS even when git is absent")
	}
}

// notFoundError satisfies the error interface and mimics exec.ErrNotFound.
type notFoundError struct{ name string }

func (e *notFoundError) Error() string { return e.name + ": executable file not found in $PATH" }

// TestCaptureGitNotARepo exercises GitAvailable=false when git is present but
// the working directory is not a repository. We redirect to a no-op git stub
// that always exits non-zero on rev-parse.
func TestCaptureGitNotARepo(t *testing.T) {
	orig := captureGit
	t.Cleanup(func() { captureGit = orig })

	// Point captureGit at a shell one-liner that exits 128 (git's "not a repo"
	// code) for rev-parse. We use /bin/sh -c so no external binary is needed.
	captureGit = func() (string, error) {
		// Return a path that when used as the git binary will fail rev-parse.
		// We do this by returning a script path we construct below.
		return "/bin/false", nil
	}

	p := Capture()
	if p.GitAvailable {
		t.Errorf("expected GitAvailable=false when rev-parse fails")
	}
	if p.GitCommit != "" {
		t.Errorf("expected GitCommit=\"\" when not in a repo")
	}
	// CapturedAt and other fields must still be set.
	if !strings.Contains(p.CapturedAt, "T") {
		t.Errorf("expected RFC3339 CapturedAt, got %q", p.CapturedAt)
	}
}
