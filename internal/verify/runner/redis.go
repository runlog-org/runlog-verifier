// Redis provisioner helpers. The reexecute orchestrator calls
// ProvisionRedisDB before each branch run to select an ephemeral
// database number, and DropRedisDB after teardown. The driver itself
// (SubprocessDriver with tool=redis always uses lang=shell) is
// stateless — it consumes the resulting $REDIS_URL from the inputs map.
//
// Trust-the-host contract: the user is responsible for having `redis-cli` on
// PATH and a server reachable via the base URL supplied here. The verifier
// follows the same trust-the-host model as sqlite (local file) and postgres
// (local or remote via psql).
//
// DB-number race tolerance: ProvisionRedisDB picks a random DB number from
// [0, 15] (Redis's fixed 16-DB range). Two parallel branch runs could collide
// 1-in-16 per pair — accepted for v0.1. In practice, mutations re-run
// sequentially today, so the collision window is only between the two
// baseline branches (failed + working), each of which FLUSHDBs before use.

package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrRedisProvision is returned when a FLUSHDB provisioning or teardown
// invocation fails. The reexecute orchestrator surfaces this as the
// typed reason `runtime_provision_failed`.
var ErrRedisProvision = errors.New("runner: redis provisioning failed")

const (
	// redisProvisionTimeout caps both FLUSHDB (provision) and FLUSHDB
	// (drop) invocations. Generous because the connecting target may be
	// on a remote host.
	redisProvisionTimeout = 30 * time.Second
)

// randomDBNum returns a random integer in [0, 15] using crypto/rand.
// Redis supports exactly 16 databases (0-15) by default.
func randomDBNum() (int, error) {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int(b[0] & 0x0F), nil
}

// ProvisionRedisDB selects a random DB number from [0, 15] and FLUSHDBs it
// to ensure cleanliness. Returns the selected DB number and a URL pointing
// at that specific database. The baseURL's path component is replaced; all
// other components (scheme, auth, host, port, query, fragment) are preserved.
//
// On failure, returns ErrRedisProvision wrapping the underlying error.
func ProvisionRedisDB(baseURL string) (dbNum int, branchURL string, err error) {
	if baseURL == "" {
		return 0, "", fmt.Errorf("%w: empty baseURL", ErrRedisProvision)
	}
	n, err := randomDBNum()
	if err != nil {
		return 0, "", fmt.Errorf("%w: random db num: %v", ErrRedisProvision, err)
	}
	dbNum = n

	bURL, err := replaceRedisURLDatabase(baseURL, dbNum)
	if err != nil {
		return 0, "", fmt.Errorf("%w: rewrite URL with db num: %v", ErrRedisProvision, err)
	}
	branchURL = bURL

	if err := runRedisCLI(branchURL, "FLUSHDB"); err != nil {
		// Best-effort cleanup — we can't do much if FLUSHDB itself failed,
		// but call DropRedisDB to keep the teardown path symmetric.
		_ = DropRedisDB(baseURL, dbNum)
		return 0, "", err
	}
	return dbNum, branchURL, nil
}

// DropRedisDB clears the ephemeral database via FLUSHDB. Best-effort: the
// caller runs this in a deferred cleanup, so a failure here only matters for
// observability. Empty baseURL is treated as a no-op (return nil).
func DropRedisDB(baseURL string, dbNum int) error {
	if baseURL == "" {
		return nil
	}
	branchURL, err := replaceRedisURLDatabase(baseURL, dbNum)
	if err != nil {
		return fmt.Errorf("%w: rewrite URL for drop: %v", ErrRedisProvision, err)
	}
	return runRedisCLI(branchURL, "FLUSHDB")
}

// replaceRedisURLDatabase returns baseURL with its path component replaced
// by /<dbNum>, preserving auth, host, port, query, and fragment.
// Accepts both redis:// and rediss:// (TLS) schemes; rejects anything else.
func replaceRedisURLDatabase(baseURL string, dbNum int) (string, error) {
	if baseURL == "" {
		return "", fmt.Errorf("%w: empty baseURL", ErrRedisProvision)
	}
	if !strings.HasPrefix(baseURL, "redis://") && !strings.HasPrefix(baseURL, "rediss://") {
		return "", fmt.Errorf("baseURL must be a redis:// or rediss:// URI, got %q", baseURL)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + strconv.Itoa(dbNum)
	return u.String(), nil
}

// runRedisCLI runs `redis-cli -u <branchURL> <args...>` with a provisioning
// timeout, capturing stderr. Non-zero exits are wrapped as ErrRedisProvision
// with the trimmed stderr message included.
func runRedisCLI(branchURL string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisProvisionTimeout)
	defer cancel()

	cmdArgs := append([]string{"-u", branchURL}, args...)
	cmd := exec.CommandContext(ctx, "redis-cli", cmdArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: redis-cli %s: %v: %s",
			ErrRedisProvision, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
