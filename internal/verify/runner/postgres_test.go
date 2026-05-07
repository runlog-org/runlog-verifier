package runner

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// skipIfNoPGURL skips when RUNLOG_VERIFY_PGURL is unset or psql isn't on
// PATH. Returns the connection string for tests that want to use it.
//
// Driver-level tests need a manually-prepared DSN that points at an
// EXISTING database (we don't provision in this layer); the value is
// passed verbatim to psql.
func skipIfNoPGURL(t *testing.T) string {
	t.Helper()
	skipIfNoBin(t, "psql")
	dsn := os.Getenv("RUNLOG_VERIFY_PGURL")
	if dsn == "" {
		t.Skip("RUNLOG_VERIFY_PGURL not set — skipping postgres test")
	}
	return dsn
}

func TestSubprocessDriver_Postgres_LangValidation(t *testing.T) {
	d := SubprocessDriver{Tool: "postgres", Workdir: newSandbox(t)}

	// lang=sql must validate; we feed a missing-$DATABASE_URL inputs map so
	// the call returns the typed missing-DSN error rather than actually
	// invoking psql. validateSteps must NOT reject sql.
	_, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "sql", Body: "SELECT 1;"}},
		nil,
		5,
	)
	// Either a validation pass + missing-DSN error (preferred) or, if a
	// future tightening rejects earlier, that's fine too — but the error
	// must wrap ErrSubprocessTool, NOT a "lang not valid" rejection that
	// names the tool.
	if err == nil {
		t.Fatalf("lang=sql under tool=postgres with no DSN: expected error, got nil")
	}
	if !errors.Is(err, ErrSubprocessTool) {
		t.Fatalf("lang=sql validation: err=%v, want wrapping ErrSubprocessTool", err)
	}
	// Make sure the failure path is the missing-DSN one, not a lang
	// rejection — i.e. the validator accepted lang=sql.
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("lang=sql under tool=postgres: err=%v, want missing-$DATABASE_URL error (validateSteps must accept lang=sql)", err)
	}

	// lang=shell must validate. We can't actually run shell here without
	// `sh`, but skipIfNoBin covers that — and the validator runs before
	// any exec so we test the validation path by triggering a guaranteed
	// validation failure when lang is unsupported, comparing against the
	// shell-passes case.
	skipIfNoBin(t, "sh")
	_, err = d.Run(
		nil,
		[]Step{{Type: "code", Lang: "shell", Body: "true"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("lang=shell under tool=postgres: unexpected err=%v", err)
	}

	// lang=bash must reject with ErrSubprocessTool — fails before any
	// psql/shell invocation, no skip-gate needed.
	_, err = d.Run(
		nil,
		[]Step{{Type: "code", Lang: "bash", Body: "true"}},
		nil,
		5,
	)
	if !errors.Is(err, ErrSubprocessTool) {
		t.Fatalf("lang=bash under tool=postgres: err=%v, want wrapping ErrSubprocessTool", err)
	}
}

func TestSubprocessDriver_Postgres_SelectOne(t *testing.T) {
	dsn := skipIfNoPGURL(t)
	d := SubprocessDriver{Tool: "postgres", Workdir: newSandbox(t)}
	res, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "sql", Body: "SELECT 1;"}},
		map[string]any{"$DATABASE_URL": dsn},
		10,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected raised: %s: %s", res.Exception, res.Message)
	}
	if !strings.Contains(res.Repr, "1") {
		t.Fatalf("repr=%q, want it to contain %q", res.Repr, "1")
	}
}

func TestSubprocessDriver_Postgres_RaiseException(t *testing.T) {
	dsn := skipIfNoPGURL(t)
	d := SubprocessDriver{Tool: "postgres", Workdir: newSandbox(t)}
	res, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "sql", Body: "DO $$ BEGIN RAISE EXCEPTION 'boom'; END; $$;"}},
		map[string]any{"$DATABASE_URL": dsn},
		10,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Raised {
		t.Fatalf("expected raised, got TypeName=%q repr=%q", res.TypeName, res.Repr)
	}
	if !strings.Contains(res.Message, "exited") && !strings.Contains(res.Message, "boom") {
		t.Fatalf("message=%q, want substring naming exit code or 'boom'", res.Message)
	}
}

func TestSubprocessDriver_Postgres_MissingDatabaseURL(t *testing.T) {
	d := SubprocessDriver{Tool: "postgres", Workdir: newSandbox(t)}
	_, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "sql", Body: "SELECT 1;"}},
		nil,
		5,
	)
	if err == nil {
		t.Fatalf("expected error for missing $DATABASE_URL, got nil")
	}
	if !errors.Is(err, ErrSubprocessTool) {
		t.Fatalf("err=%v, want wrapping ErrSubprocessTool", err)
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err=%v, want substring '$DATABASE_URL'", err)
	}
}
