// Docker provisioner helpers. The reexecute orchestrator calls
// ProvisionDockerSandbox before each branch run to mint a per-branch
// resource-name prefix, and CleanDockerSandbox after teardown to walk
// prefix-matched containers / images / networks / buildx builders and
// remove them best-effort. The driver itself (SubprocessDriver with
// tool=docker always uses lang=shell) is stateless — it consumes the
// resulting $DOCKER_PREFIX from the inputs map.
//
// Trust-the-host contract: the user is responsible for having `docker` on
// PATH and a daemon reachable via the default socket / DOCKER_HOST env.
// The verifier follows the same trust-the-host model as sqlite (local
// file), postgres (psql), and redis (redis-cli).
//
// Provisioning is *not* daemon-mutating: ProvisionDockerSandbox runs a
// `docker version` reachability probe and mints a prefix; nothing is
// created on the daemon at provision time. The cassette's setup_script
// and action steps are responsible for composing the prefix into their
// own resource names ($DOCKER_PREFIX-img, $DOCKER_PREFIX-builder, …),
// which the cleanup walk will then sweep.

package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrDockerProvision is returned when a docker provisioning or cleanup
// invocation fails. The reexecute orchestrator surfaces this as the
// typed reason `runtime_provision_failed`.
var ErrDockerProvision = errors.New("runner: docker provisioning failed")

// dockerSandboxPrefix marks every per-branch sandbox prefix so manual
// sweeps and dashboards can identify them
// (`docker ps -aq --filter "name=^runlog-verify-" | xargs docker rm -f`).
const dockerSandboxPrefix = "runlog-verify-"

// randomSandboxSuffix returns 8 hex chars (32 bits of entropy) from
// crypto/rand. Smaller than postgres' 16-hex suffix because docker
// resources have no fixed slot range — collision risk is bounded by the
// number of concurrent verifier invocations, and 4.3 billion combinations
// is more than enough for any realistic concurrency level. Matches docker's
// own tendency to truncate IDs to 12 chars in display.
func randomSandboxSuffix() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ProvisionDockerSandbox probes daemon reachability via
// `docker version --format '{{.Server.Version}}'`, then mints a unique
// sandbox prefix of the form `runlog-verify-<8-hex>`. NOTHING is created
// on the daemon at provision time — the prefix is just a namespace seeds
// compose into their own resource names.
//
// On failure, returns ErrDockerProvision wrapping the underlying probe
// stderr or random-suffix error.
func ProvisionDockerSandbox() (sandboxID string, err error) {
	if err := execProvisionCLI("docker",
		[]string{"version", "--format", "{{.Server.Version}}"},
		ErrDockerProvision, "docker version probe"); err != nil {
		return "", err
	}
	suffix, err := randomSandboxSuffix()
	if err != nil {
		return "", fmt.Errorf("%w: random suffix: %v", ErrDockerProvision, err)
	}
	return dockerSandboxPrefix + suffix, nil
}

// CleanDockerSandbox walks prefix-matched docker resources and removes them
// best-effort. Each step's failure is collected via errors.Join but doesn't
// abort the next step, so a transient daemon hiccup on (say) the network-rm
// pass still lets the buildx-rm pass run.
//
// Order matters: containers first (so their image/network/builder refs are
// released), then images, then networks, then buildx builders. Empty
// sandboxID is treated as a no-op (return nil) so the orchestrator's
// teardown can call this unconditionally.
//
// Containers / images / networks all share the same shape (list-by-filter
// → rm-by-id) so they go through cleanDockerByFilter; buildx is its own
// path because `docker buildx ls` doesn't accept --filter and treats list
// failure as a clean no-op rather than an error.
func CleanDockerSandbox(sandboxID string) error {
	if sandboxID == "" {
		return nil
	}
	var errs []error
	cleanups := []struct {
		label    string // diagnostic label, also used in the rm command label
		listArgs []string
		rmArgs   []string // prefixed onto the resource id
	}{
		{
			label:    "containers",
			listArgs: []string{"ps", "-aq", "--filter", "name=^" + sandboxID},
			rmArgs:   []string{"rm", "-f"},
		},
		{
			label:    "images",
			listArgs: []string{"images", "-q", "--filter", "reference=" + sandboxID + "*"},
			rmArgs:   []string{"rmi", "-f"},
		},
		{
			label:    "networks",
			listArgs: []string{"network", "ls", "-q", "--filter", "name=^" + sandboxID},
			rmArgs:   []string{"network", "rm"},
		},
	}
	for _, c := range cleanups {
		if err := cleanDockerByFilter(sandboxID, c.label, c.listArgs, c.rmArgs); err != nil {
			errs = append(errs, err)
		}
	}
	if err := cleanDockerBuildxBuilders(sandboxID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// cleanDockerByFilter implements the shared "list resources matching a
// sandboxID-derived filter, then `docker rm` each id" shape used by
// containers, images, and networks. List-failure surfaces as
// ErrDockerProvision; per-id rm failures abort the loop with the wrapped
// CLI error.
func cleanDockerByFilter(sandboxID, label string, listArgs, rmArgs []string) error {
	ids, err := dockerListResources(listArgs[0], listArgs[1:])
	if err != nil {
		return fmt.Errorf("%w: list %s for %q: %v",
			ErrDockerProvision, label, sandboxID, err)
	}
	for _, id := range ids {
		full := append(append([]string(nil), rmArgs...), id)
		if err := execProvisionCLI("docker", full,
			ErrDockerProvision, "docker "+strings.Join(full, " ")); err != nil {
			return err
		}
	}
	return nil
}

// cleanDockerBuildxBuilders removes every buildx builder whose name starts
// with sandboxID. `docker buildx ls` doesn't accept a `--filter` flag, so
// we list all builders via `--format '{{.Name}}'` and prefix-match in Go.
func cleanDockerBuildxBuilders(sandboxID string) error {
	names, err := dockerListResources("buildx",
		[]string{"ls", "--format", "{{.Name}}"})
	if err != nil {
		// buildx may not be installed (e.g. minimal docker daemon); treat
		// enumeration failure as a clean no-op rather than aborting the
		// rest of the cleanup chain.
		return nil
	}
	for _, name := range names {
		if !strings.HasPrefix(name, sandboxID) {
			continue
		}
		if err := execProvisionCLI("docker", []string{"buildx", "rm", name},
			ErrDockerProvision, "docker buildx rm "+name); err != nil {
			return err
		}
	}
	return nil
}

// dockerListResources runs `docker <subcommand> <args...>` and returns the
// non-empty lines from stdout. The provisioning timeout caps every
// invocation. Used by the cleanXxx helpers above.
//
// Uses its own exec helper rather than execProvisionCLI because the latter
// discards stdout (it's tuned for side-effect-only provisioning CLIs); the
// list helpers need stdout to enumerate matched resource ids.
func dockerListResources(subcommand string, args []string) ([]string, error) {
	all := append([]string{subcommand}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), provisionCLITimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", all...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s: %v: %s",
			strings.Join(all, " "), err, strings.TrimSpace(stderr.String()))
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}
