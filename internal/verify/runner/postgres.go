// Postgres provisioner helpers. The reexecute orchestrator calls
// ProvisionPostgresDB before each branch run to create an ephemeral
// database, and DropPostgresDB after teardown. The driver itself
// (SubprocessDriver execPostgres in subprocess.go) is stateless — it
// consumes the resulting $DATABASE_URL from the inputs map.
//
// Trust-the-host contract: the user is responsible for having `psql` on
// PATH and a server reachable via the base DSN supplied here. The
// runlog_verify_ prefix on every provisioned DB makes manual stale-DB
// sweeps straightforward (`SELECT datname FROM pg_database WHERE
// datname LIKE 'runlog_verify_%'`).

package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// ErrPostgresProvision is returned when a CREATE DATABASE / DROP DATABASE
// invocation fails. The reexecute orchestrator surfaces this as the
// typed reason `runtime_provision_failed`.
var ErrPostgresProvision = errors.New("runner: postgres provisioning failed")

const (
	// postgresProvisionTimeout caps both CREATE and DROP invocations.
	// Generous because the connecting role may be on a remote host.
	postgresProvisionTimeout = 30 * time.Second

	// postgresDBPrefix marks every ephemeral database created by this
	// verifier so manual sweeps and dashboards can identify them.
	postgresDBPrefix = "runlog_verify_"
)

// randomDBSuffix returns 16 hex chars from crypto/rand, suitable as a
// database-name suffix. 64 bits of entropy is enough that two parallel
// branches racing on provisioning will never collide.
func randomDBSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ProvisionPostgresDB creates a fresh ephemeral database on the server
// reachable via baseDSN and returns the new DB's name plus a connection
// string pointing at it. The baseDSN's path component is replaced; all
// other components (host, port, user, password, query params) are
// preserved.
//
// On failure, returns ErrPostgresProvision wrapping the underlying psql
// stderr or DSN-parse error.
func ProvisionPostgresDB(baseDSN string) (dbName, branchDSN string, err error) {
	if baseDSN == "" {
		return "", "", fmt.Errorf("%w: empty baseDSN", ErrPostgresProvision)
	}
	suffix, err := randomDBSuffix()
	if err != nil {
		return "", "", fmt.Errorf("%w: random suffix: %v", ErrPostgresProvision, err)
	}
	dbName = postgresDBPrefix + suffix

	if err := runPsqlAdmin(baseDSN, fmt.Sprintf(`CREATE DATABASE "%s"`, dbName)); err != nil {
		return "", "", err
	}

	branchDSN, err = replaceDSNDatabase(baseDSN, dbName)
	if err != nil {
		// Best-effort drop the DB we just created; we can't return it to
		// the caller anyway.
		_ = DropPostgresDB(baseDSN, dbName)
		return "", "", fmt.Errorf("%w: rewrite DSN with new db: %v", ErrPostgresProvision, err)
	}
	return dbName, branchDSN, nil
}

// DropPostgresDB removes an ephemeral database. Best-effort: the caller
// runs this in a deferred cleanup, so a failure here only matters for
// observability — the next stale-DB sweep will get it.
func DropPostgresDB(baseDSN, dbName string) error {
	if dbName == "" {
		return nil
	}
	return runPsqlAdmin(baseDSN, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, dbName))
}

// runPsqlAdmin executes a single SQL command via psql against baseDSN.
// CREATE/DROP DATABASE statements cannot run inside transactions, so we
// pass them via -c rather than stdin (psql -c implicitly autocommits
// each statement).
func runPsqlAdmin(baseDSN, sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresProvisionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "psql",
		"--dbname="+baseDSN,
		"--no-psqlrc", "-X", "-q",
		"-v", "ON_ERROR_STOP=1",
		"-c", sql,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s: %v: %s",
			ErrPostgresProvision, sql, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// replaceDSNDatabase returns baseDSN with its path component (the
// database name) replaced by dbName, preserving every other field.
// Accepts both URI form (postgres://user:pw@host:port/db?opts) and
// keyword form (libpq-style "host=... dbname=..."), but the keyword form
// is normalised through net/url first by recognising the "postgres://"
// prefix.
func replaceDSNDatabase(baseDSN, dbName string) (string, error) {
	if !strings.HasPrefix(baseDSN, "postgres://") &&
		!strings.HasPrefix(baseDSN, "postgresql://") {
		return "", fmt.Errorf("baseDSN must be a postgres:// URI (libpq keyword form not supported)")
	}
	u, err := url.Parse(baseDSN)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbName
	return u.String(), nil
}
