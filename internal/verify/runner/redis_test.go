package runner

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// skipIfNoREDISURL skips when RUNLOG_VERIFY_REDISURL is unset or redis-cli
// isn't on PATH. Returns the Redis URL for tests that want to use it.
func skipIfNoREDISURL(t *testing.T) string {
	t.Helper()
	skipIfNoBin(t, "redis-cli")
	u := os.Getenv("RUNLOG_VERIFY_REDISURL")
	if u == "" {
		t.Skip("RUNLOG_VERIFY_REDISURL not set — skipping redis test")
	}
	return u
}

func TestReplaceRedisURLDatabase(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		dbNum   int
		want    string
		wantErr string // non-empty substring expected in error
	}{
		{
			name:    "redis plain localhost",
			baseURL: "redis://localhost:6379/0",
			dbNum:   3,
			want:    "redis://localhost:6379/3",
		},
		{
			name:    "rediss with auth query and port",
			baseURL: "rediss://user:pw@host:6380/?ssl=true",
			dbNum:   7,
			want:    "rediss://user:pw@host:6380/7?ssl=true",
		},
		{
			name:    "bad scheme http",
			baseURL: "http://x",
			wantErr: "redis:// or rediss://",
		},
		{
			name:    "empty input",
			baseURL: "",
			wantErr: "empty baseURL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := replaceRedisURLDatabase(tc.baseURL, tc.dbNum)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%q, want=%q", got, tc.want)
			}
		})
	}
}

func TestProvisionAndDropRedisDB(t *testing.T) {
	baseURL := skipIfNoREDISURL(t)

	// Three-tier skip gate: reachability probe so a stale env var produces
	// a clean SKIP rather than a misleading FAIL.
	probe := exec.Command("redis-cli", "-u", baseURL, "PING")
	probe.Stderr = nil
	probe.Stdout = nil
	if err := probe.Run(); err != nil {
		t.Skipf("RUNLOG_VERIFY_REDISURL %q is set but server is unreachable: %v", baseURL, err)
	}

	dbNum, branchURL, err := ProvisionRedisDB(baseURL)
	if err != nil {
		t.Fatalf("ProvisionRedisDB: %v", err)
	}
	// Always drop, even on subsequent test failure.
	t.Cleanup(func() { _ = DropRedisDB(baseURL, dbNum) })

	if dbNum < 0 || dbNum > 15 {
		t.Fatalf("dbNum=%d, want ∈ [0, 15]", dbNum)
	}
	wantSuffix := "/" + strings.TrimPrefix(branchURL[strings.LastIndex(branchURL, "/"):], "/")
	if !strings.Contains(branchURL, "/"+strings.TrimPrefix(wantSuffix, "/")) {
		t.Fatalf("branchURL=%q does not contain /%d", branchURL, dbNum)
	}

	// SET a key, GET it back.
	setCmd := exec.Command("redis-cli", "-u", branchURL, "SET", "runlog_test_k", "hello")
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("SET: %v: %s", err, out)
	}
	getCmd := exec.Command("redis-cli", "-u", branchURL, "GET", "runlog_test_k")
	out, err := getCmd.Output()
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if !strings.Contains(strings.TrimSpace(string(out)), "hello") {
		t.Fatalf("GET returned %q, want 'hello'", strings.TrimSpace(string(out)))
	}

	// Drop the DB, verify key is gone.
	if err := DropRedisDB(baseURL, dbNum); err != nil {
		t.Fatalf("DropRedisDB: %v", err)
	}
	getCmd2 := exec.Command("redis-cli", "-u", branchURL, "GET", "runlog_test_k")
	out2, err := getCmd2.Output()
	if err != nil {
		t.Fatalf("GET after drop: %v", err)
	}
	// After FLUSHDB, GET of a missing key returns "" (empty line).
	trimmed := strings.TrimSpace(string(out2))
	if trimmed != "" && trimmed != "(nil)" {
		t.Fatalf("after DropRedisDB, GET returned %q, want empty or (nil)", trimmed)
	}
}
