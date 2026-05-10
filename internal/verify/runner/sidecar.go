// Sidecar driver. Backs the schema's `kind: sidecar_process` arm under
// cassette.fixtures by spawning a long-running auxiliary process whose
// lifecycle spans a single branch action, then tearing it down.
//
// Unlike the other drivers in this package (subprocess, python, postgres,
// redis, docker), SidecarDriver does NOT execute the action itself —
// it spawns a background process that the action depends on (e.g. a mock
// HTTP server, a vendored daemon under test) and stays running until the
// orchestrator calls Stop.
//
// One driver value handles one Start/Stop cycle. The orchestrator
// constructs a fresh driver per branch + per mutation re-run, so there
// is no cross-call state on the driver itself; runtime state (process
// handle, stderr ring buffer, stop signal) lives on SidecarHandle.

package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// SidecarConfig is the parsed shape of one fixture_sidecar_process entry
// from cassette.fixtures. The orchestrator decodes the raw map[string]any
// from cassette into this struct in T4; the driver consumes it as-is and
// does not re-validate (orchestrator is the trust boundary for shape).
type SidecarConfig struct {
	// Name is the $FIXTURE_* token (key in cassette.fixtures) — used only
	// for diagnostic messages; the driver does not interpret it.
	Name string
	// Command is the argv array, after $-token substitution by the
	// orchestrator. Must be non-empty (Start returns ErrSidecarStartup
	// when len(Command) == 0).
	Command []string
	// StopGraceSeconds is the SIGTERM-then-SIGKILL grace period. Schema
	// bounds: [0, 60]; default 5. The driver itself uses 5 when the
	// caller passes 0 to keep teardown bounded even if the orchestrator
	// forgot to apply the schema default.
	StopGraceSeconds int
	// ReadyWhen is the discriminated readiness signal — exactly one
	// field non-zero in a valid config. The driver checks them in
	// priority order (stderr_regex → path_exists → delay_seconds) and
	// falls through to a fixed short delay if all three are zero.
	ReadyWhen ReadyWhen
}

// ReadyWhen is the discriminated union for the schema's ready_when
// property. Exactly one field is non-zero in a valid SidecarConfig
// (the orchestrator validates that on decode; SidecarDriver checks them
// in priority order stderr_regex → path_exists → delay_seconds, falling
// through to a fixed short delay if all three are zero — matches the
// schema's "use when the process has no observable readiness signal"
// intent).
type ReadyWhen struct {
	// StderrRegex is compiled per Start; empty means "not used". A
	// match anywhere on a stderr line — buffered by a bufio.Scanner
	// reading the same stream that fills the ring buffer — flips the
	// handle to ready.
	StderrRegex string
	// PathExists is the post-substitution path the driver polls (50ms
	// cadence) for existence. Absolute or relative; relative paths
	// resolve against the orchestrator's cwd, which the driver does
	// not change.
	PathExists string
	// DelaySeconds is the schema-bounded fixed sleep [0, 60] before
	// the handle flips to ready. A zero value with the other two also
	// zero means "no observable readiness signal" — fall through to
	// the fixed 200ms fallback.
	DelaySeconds float64
}

// SidecarDriver spawns and manages a long-running sidecar process. One
// driver value handles one start-stop cycle; the orchestrator constructs
// a fresh driver for each branch + each mutation re-run.
type SidecarDriver struct{}

// SidecarHandle holds runtime state for an in-flight sidecar — the
// process, captured stderr ring buffer (8 KiB), and the stop signal.
// Returned by SidecarDriver.Start; the orchestrator owns the value and
// calls Stop / StderrTail on it.
type SidecarHandle struct {
	Name string
	PID  int

	cmd    *exec.Cmd
	cancel context.CancelFunc

	// stopGrace is captured at Start time from SidecarConfig so the
	// orchestrator's Stop call doesn't need to thread cfg through.
	stopGrace time.Duration

	// waitDone closes when the process has exited (Cmd.Wait returned).
	// waitErr captures whatever Cmd.Wait returned (nil for clean exit,
	// *exec.ExitError for non-zero, etc.).
	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error

	// stderr ring buffer — fixed-size byte slice with a write-position
	// cursor and a "wrapped" flag. StderrTail splices [pos:]+[:pos]
	// when wrapped, else returns [:pos]. The drain goroutine writes
	// under stderrMu so concurrent StderrTail() reads see a coherent
	// snapshot.
	stderrMu  sync.Mutex
	stderrBuf []byte
	stderrPos int
	stderrWrp bool

	// stopOnce + stopErr make Stop idempotent: the second call returns
	// the first call's outcome verbatim without re-signaling.
	stopOnce sync.Once
	stopErr  error
}

// Sentinels — orchestrator surfaces these as typed reasons in T4.
var (
	// ErrSidecarStartup wraps any failure during Start: exec.LookPath
	// failure, fork failure, or the sidecar exiting before its
	// readiness signal fires. The wrapped error message includes the
	// stderr tail so the orchestrator can attach it to the typed
	// reason without re-fetching from the (already-defunct) handle.
	ErrSidecarStartup = errors.New("runner: sidecar startup failed")
	// ErrSidecarReadyTimeout is returned when none of the readiness
	// signals fire within sidecarStartupTimeout. The driver kills the
	// sidecar before returning so the caller can't accidentally leak it.
	ErrSidecarReadyTimeout = errors.New("runner: sidecar ready signal not observed within timeout")
	// ErrSidecarStopTimeout is returned when SIGTERM + SIGKILL both
	// fail to bring the process down within the grace + 1s window.
	// Indicates a stuck process or a signal-resistant child.
	ErrSidecarStopTimeout = errors.New("runner: sidecar did not stop within grace period")
)

// sidecarStartupTimeout caps how long Start waits for the readiness
// signal before giving up. Generous on purpose — container-attached
// or DB-attached sidecars may take seconds to settle, and the schema
// only bounds the per-step action timeout, not fixture warmup.
const sidecarStartupTimeout = 30 * time.Second

// sidecarStartupTimeoutOverride lets tests inject a shorter timeout
// without dragging the production cap down. Production is nil; tests
// set it to a short duration in withTestEnv-style helpers and reset
// after the test. Mirror the package-level test-seam pattern used by
// the F73 register-flow tests (see KNOWLEDGE.md).
var sidecarStartupTimeoutOverride *time.Duration

// sidecarStderrRingSize is the per-handle cap on captured stderr (8
// KiB). Mirrors the existing subprocess driver's truncation convention
// — long enough to surface a useful error tail, small enough to keep
// the in-process buffer cheap when many sidecars run in sequence.
const sidecarStderrRingSize = 8 * 1024

// sidecarReadyPollInterval is the cadence for path_exists polling and
// the regex/exit watcher loop. Tight enough to feel responsive, loose
// enough that a never-ready sidecar costs ~600 wakeups across the full
// 30s startup timeout.
const sidecarReadyPollInterval = 50 * time.Millisecond

// sidecarFallbackDelay is the fixed sleep used when ready_when has
// none of stderr_regex / path_exists / delay_seconds set — matches the
// schema's "no observable readiness signal" fallback intent.
const sidecarFallbackDelay = 200 * time.Millisecond

// sidecarKillTimeout is the additional wait after SIGKILL before Stop
// gives up and returns ErrSidecarStopTimeout. SIGKILL on a live process
// is delivered by the kernel directly so 1s is generous.
const sidecarKillTimeout = 1 * time.Second

// Start spawns the sidecar, waits for the ready_when condition, and
// returns a handle. env is the per-branch input env merged with the
// host's PATH/HOME etc; the driver passes it through to exec.Cmd.Env
// without re-validating (orchestrator handles reserved-name guarding).
//
// Failure modes (each wraps the appropriate sentinel):
//   - empty Command, exec.LookPath, or fork failure → ErrSidecarStartup
//   - process exits before ready signal observed   → ErrSidecarStartup
//     (with stderr tail included in the error message)
//   - ready_when fires no signal within the
//     startup timeout                                → ErrSidecarReadyTimeout
//     (sidecar killed before return so caller can't
//     leak it)
func (SidecarDriver) Start(ctx context.Context, cfg SidecarConfig, env []string) (*SidecarHandle, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("%w: %s: empty command", ErrSidecarStartup, cfg.Name)
	}

	stopGrace := time.Duration(cfg.StopGraceSeconds) * time.Second
	if stopGrace <= 0 {
		stopGrace = 5 * time.Second
	}

	// Compile the regex up front so we surface a malformed pattern as a
	// startup error rather than silently treating it as "never matches".
	var stderrRE *regexp.Regexp
	if cfg.ReadyWhen.StderrRegex != "" {
		re, err := regexp.Compile(cfg.ReadyWhen.StderrRegex)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: compile ready_when.stderr_regex: %v",
				ErrSidecarStartup, cfg.Name, err)
		}
		stderrRE = re
	}

	// Use a child context so we can kill the process via Cmd.Process.Kill
	// later — CommandContext's own cancellation also signals the process
	// when the parent ctx hits its deadline, which is the behavior we want
	// for graceful shutdown on caller cancellation.
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Env = env

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %s: stderr pipe: %v", ErrSidecarStartup, cfg.Name, err)
	}
	// We don't need stdout — discard it so the OS pipe buffer doesn't fill
	// and block the child's writes.
	cmd.Stdout = io.Discard

	h := &SidecarHandle{
		Name:      cfg.Name,
		cmd:       cmd,
		cancel:    cancel,
		stopGrace: stopGrace,
		waitDone:  make(chan struct{}),
		stderrBuf: make([]byte, sidecarStderrRingSize),
	}

	// Stderr drain goroutine: pumps the child's stderr into both a
	// scanner (for regex matching) and the ring buffer. Closes the
	// matched channel when the regex first matches; nil regex means
	// the channel stays open and only the exit/path watchers can flip
	// the handle to ready.
	matched := make(chan struct{})
	var matchOnce sync.Once
	signalMatched := func() { matchOnce.Do(func() { close(matched) }) }

	go func() {
		defer stderrPipe.Close()
		// The bufio.Scanner gives us line-oriented regex matching while
		// we still get every byte appended to the ring buffer (we copy
		// each line + a trailing newline).
		scanner := bufio.NewScanner(stderrPipe)
		// Allow up to 1 MiB lines so a chatty sidecar with no newlines
		// doesn't deadlock the scanner; ring buffer caps total bytes
		// retained anyway.
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			h.appendStderr(line)
			h.appendStderr([]byte{'\n'})
			if stderrRE != nil && stderrRE.Match(line) {
				signalMatched()
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %s: start: %v", ErrSidecarStartup, cfg.Name, err)
	}
	h.PID = cmd.Process.Pid

	// Wait goroutine: closes waitDone when the process exits. Stored
	// once via sync.Once so Stop's parallel wait is a no-op rather than
	// a duplicate Cmd.Wait (which would panic).
	go func() {
		err := cmd.Wait()
		h.waitOnce.Do(func() {
			h.waitErr = err
			close(h.waitDone)
		})
	}()

	timeout := sidecarStartupTimeout
	if sidecarStartupTimeoutOverride != nil {
		timeout = *sidecarStartupTimeoutOverride
	}

	if err := h.waitForReady(cfg.ReadyWhen, stderrRE, matched, timeout); err != nil {
		// Kill before returning so the caller can't leak the sidecar.
		// Cancel the command context first (best-effort signal), then
		// fall back to an explicit Kill if the wait channel doesn't
		// fire within the kill timeout.
		h.killAndWait()
		return nil, err
	}

	return h, nil
}

// waitForReady implements the discriminated dispatch described on
// ReadyWhen. Returns nil when the condition fires, ErrSidecarStartup
// (wrapping cmd.Wait's error) when the process exits early, or
// ErrSidecarReadyTimeout when the deadline passes.
func (h *SidecarHandle) waitForReady(rw ReadyWhen, stderrRE *regexp.Regexp, matched chan struct{}, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	// Priority dispatch: stderr_regex > path_exists > delay_seconds >
	// fallback. The orchestrator validates that exactly one is set, but
	// we tolerate multiple set fields by picking the highest-priority
	// one; the others are ignored. This matches "trust but don't trust
	// blindly" — the orchestrator is the authority on shape, but a
	// rogue config never hangs the driver in an invalid combinator.
	switch {
	case stderrRE != nil:
		// Wait for the drain goroutine to close `matched`, the process
		// to exit early, or the deadline to fire.
		for {
			select {
			case <-matched:
				return nil
			case <-h.waitDone:
				return fmt.Errorf("%w: %s: process exited before ready (err=%v, stderr=%q)",
					ErrSidecarStartup, h.Name, h.waitErr, h.StderrTail())
			case <-deadline.C:
				return fmt.Errorf("%w: %s: stderr_regex %q not matched in %s",
					ErrSidecarReadyTimeout, h.Name, rw.StderrRegex, timeout)
			}
		}
	case rw.PathExists != "":
		ticker := time.NewTicker(sidecarReadyPollInterval)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(rw.PathExists); err == nil {
				return nil
			}
			select {
			case <-h.waitDone:
				return fmt.Errorf("%w: %s: process exited before ready (err=%v, stderr=%q)",
					ErrSidecarStartup, h.Name, h.waitErr, h.StderrTail())
			case <-deadline.C:
				return fmt.Errorf("%w: %s: path %q did not exist in %s",
					ErrSidecarReadyTimeout, h.Name, rw.PathExists, timeout)
			case <-ticker.C:
				// retry os.Stat
			}
		}
	case rw.DelaySeconds > 0:
		delay := time.Duration(rw.DelaySeconds * float64(time.Second))
		return h.sleepOrExit(delay)
	default:
		return h.sleepOrExit(sidecarFallbackDelay)
	}
}

// sleepOrExit sleeps for d, returning ErrSidecarStartup if the process
// dies before the sleep finishes. Used by the delay_seconds and
// fallback ready_when paths.
func (h *SidecarHandle) sleepOrExit(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.waitDone:
		return fmt.Errorf("%w: %s: process exited before ready (err=%v, stderr=%q)",
			ErrSidecarStartup, h.Name, h.waitErr, h.StderrTail())
	case <-t.C:
		return nil
	}
}

// appendStderr writes b into the ring buffer under the mutex. When the
// write would overflow the cap, the buffer wraps — older bytes are
// silently dropped. Callers always wrap; we never grow the slice.
func (h *SidecarHandle) appendStderr(b []byte) {
	if len(b) == 0 {
		return
	}
	h.stderrMu.Lock()
	defer h.stderrMu.Unlock()

	// If the chunk is bigger than the ring, only the trailing
	// sidecarStderrRingSize bytes can possibly survive — copy that
	// suffix into the buffer, mark wrapped, reset pos to 0 (the buffer
	// is now "fully written and just wrapped").
	if len(b) >= len(h.stderrBuf) {
		copy(h.stderrBuf, b[len(b)-len(h.stderrBuf):])
		h.stderrPos = 0
		h.stderrWrp = true
		return
	}

	// Normal append: write up to the end of the slice, wrap if needed,
	// finish at the front. cap == len(h.stderrBuf) always.
	first := copy(h.stderrBuf[h.stderrPos:], b)
	h.stderrPos += first
	if h.stderrPos == len(h.stderrBuf) {
		h.stderrPos = 0
		h.stderrWrp = true
	}
	if first < len(b) {
		// Wrapped — write the remainder to the front.
		rem := b[first:]
		n := copy(h.stderrBuf, rem)
		h.stderrPos = n
		h.stderrWrp = true
	}
}

// StderrTail returns the trailing bytes (≤ sidecarStderrRingSize) of
// the sidecar's stderr. Used by the orchestrator to attach diagnostic
// context to typed reasons when Start fails OR when an action fails
// and the sidecar's output might explain why. Never returns more than
// the cap.
func (h *SidecarHandle) StderrTail() []byte {
	h.stderrMu.Lock()
	defer h.stderrMu.Unlock()
	if !h.stderrWrp {
		out := make([]byte, h.stderrPos)
		copy(out, h.stderrBuf[:h.stderrPos])
		return out
	}
	out := make([]byte, len(h.stderrBuf))
	n := copy(out, h.stderrBuf[h.stderrPos:])
	copy(out[n:], h.stderrBuf[:h.stderrPos])
	return out
}

// Stop sends SIGTERM and waits up to cfg.StopGraceSeconds (captured at
// Start time onto the handle). On grace expiry, sends SIGKILL and waits
// a further sidecarKillTimeout, then returns ErrSidecarStopTimeout. A
// cleanly exited process returns nil regardless of exit status — Stop's
// job is just to ensure the process is gone, not to surface its exit
// code.
//
// Stop is idempotent — calling it on an already-stopped handle returns
// the first call's outcome without re-signaling.
func (h *SidecarHandle) Stop() error {
	h.stopOnce.Do(func() {
		h.stopErr = h.stopOnceImpl()
	})
	return h.stopErr
}

// stopOnceImpl is the body of Stop, run at most once. Mutating-shared-
// state isolation lives on stopOnce in the public Stop wrapper; this
// helper assumes single-call semantics.
func (h *SidecarHandle) stopOnceImpl() error {
	// Already exited? Cancel the command context (releases pipes) and
	// return clean.
	select {
	case <-h.waitDone:
		h.cancel()
		return nil
	default:
	}

	// Polite shutdown: SIGTERM + grace.
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Signal(syscall.SIGTERM)
	}
	graceTimer := time.NewTimer(h.stopGrace)
	defer graceTimer.Stop()
	select {
	case <-h.waitDone:
		h.cancel()
		return nil
	case <-graceTimer.C:
	}

	// Grace expired — SIGKILL and short follow-up wait.
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	killTimer := time.NewTimer(sidecarKillTimeout)
	defer killTimer.Stop()
	select {
	case <-h.waitDone:
		h.cancel()
		return nil
	case <-killTimer.C:
		h.cancel()
		return fmt.Errorf("%w: %s (pid=%d) ignored SIGTERM+SIGKILL within %s",
			ErrSidecarStopTimeout, h.Name, h.PID, h.stopGrace+sidecarKillTimeout)
	}
}

// killAndWait is the failure-path teardown used by Start when ready
// detection fails — sends SIGKILL immediately (no grace period — the
// process is already misbehaving) and waits up to sidecarKillTimeout
// for the wait goroutine to observe the exit. Best-effort; ignores
// errors because the caller is already returning a startup-time error.
func (h *SidecarHandle) killAndWait() {
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	t := time.NewTimer(sidecarKillTimeout)
	defer t.Stop()
	select {
	case <-h.waitDone:
	case <-t.C:
	}
	h.cancel()
}
