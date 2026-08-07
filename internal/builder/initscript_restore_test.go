package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// restoreSeedConf carries an explicit appendonly yes so the tests can assert
// which appendonly directive wins after the script ran.
const restoreSeedConf = seedRedisConf + "appendonly yes\nsave 900 1\n"

// runRestoreInitScript mirrors runInitScript but lets the caller seed the data
// dir before the run and returns it for inspection, which the shared helper
// cannot do. Kept separate so restore cases do not reshape the shared helper
// under other tests.
func runRestoreInitScript(t *testing.T, hostname string, env map[string]string, seed func(dataDir string)) (conf, output, dataDir string, err error) {
	t.Helper()

	script := testInitScript()
	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	etcDir := filepath.Join(dir, "etc")
	for _, d := range []string{dataDir, etcDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(etcDir, "redis.conf"), []byte(restoreSeedConf), 0o644); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(dataDir)
	}

	s := strings.ReplaceAll(script, "/etc/redis", etcDir)
	s = strings.ReplaceAll(s, "/data", dataDir)
	s = strings.ReplaceAll(s, "exec redis-server", "echo WOULD_EXEC redis-server")
	s = strings.ReplaceAll(s, "sleep 2", "sleep 0")

	path := filepath.Join(dir, "init.sh")
	if err := os.WriteFile(path, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}

	stub, err := filepath.Abs("testdata/bin")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", path)
	cmd.Env = []string{
		"PATH=" + stub + ":" + os.Getenv("PATH"),
		"HOSTNAME=" + hostname,
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, runErr := cmd.CombinedOutput()

	b, readErr := os.ReadFile(filepath.Join(dataDir, "redis.conf"))
	if readErr != nil {
		return "", string(out), dataDir, runErr
	}
	return string(b), string(out), dataDir, runErr
}

// masterEnv makes the sentinel stub confirm this pod as the master, which is
// the topology a restore restart runs under.
func masterEnv() map[string]string {
	return map[string]string{
		"REDGUARD_TEST_MASTER_IP": "10.244.0.5",
		"REDGUARD_TEST_MY_IP":     "10.244.0.5",
	}
}

// lastAppendonlyDirective returns the value of the last appendonly line, which
// is the one redis-server applies.
func lastAppendonlyDirective(conf string) string {
	last := ""
	for _, line := range strings.Split(conf, "\n") {
		if v, ok := strings.CutPrefix(line, "appendonly "); ok {
			last = v
		}
	}
	return last
}

func TestInitScript_RestorePayload_LoadedAndAOFDisabled(t *testing.T) {
	payload := []byte("REDIS0011-restore-payload")

	conf, out, dataDir, err := runRestoreInitScript(t, "test-redis-0", masterEnv(), func(dataDir string) {
		if err := os.WriteFile(filepath.Join(dataDir, "redguard-restore.rdb"), payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dataDir, "appendonlydir"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "appendonlydir", "appendonly.aof.1.incr.aof"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	dump, readErr := os.ReadFile(filepath.Join(dataDir, "dump.rdb"))
	if readErr != nil {
		t.Fatalf("restore payload was not moved to dump.rdb: %v", readErr)
	}
	if string(dump) != string(payload) {
		t.Errorf("dump.rdb content = %q, want the staged payload", dump)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "redguard-restore.rdb")); !os.IsNotExist(statErr) {
		t.Error("staged payload must be consumed; its absence is the controller's restart signal")
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "appendonlydir")); !os.IsNotExist(statErr) {
		t.Error("old AOF dir left in place; redis-server would recreate state from it after appendonly is re-enabled")
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "appendonlydir.pre-restore", "appendonly.aof.1.incr.aof")); statErr != nil {
		t.Errorf("old AOF must be kept aside as a rollback artifact: %v", statErr)
	}
	if got := lastAppendonlyDirective(conf); got != "no" {
		t.Errorf("effective appendonly directive = %q, want no; with AOF on, redis 7 never loads dump.rdb", got)
	}
	if !strings.Contains(out, "WOULD_EXEC redis-server") {
		t.Errorf("script must reach the exec, output:\n%s", out)
	}
}

func TestInitScript_RestorePayload_ReplacesStalePreRestoreDir(t *testing.T) {
	_, out, dataDir, err := runRestoreInitScript(t, "test-redis-0", masterEnv(), func(dataDir string) {
		if err := os.WriteFile(filepath.Join(dataDir, "redguard-restore.rdb"), []byte("REDIS0011"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{"appendonlydir", "appendonlydir.pre-restore"} {
			if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dataDir, "appendonlydir.pre-restore", "stale"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	// A stale dir from an earlier restore must not swallow the new move
	// (mv dir onto an existing dir nests instead of replacing).
	if _, statErr := os.Stat(filepath.Join(dataDir, "appendonlydir.pre-restore", "stale")); !os.IsNotExist(statErr) {
		t.Error("stale pre-restore dir was not replaced")
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "appendonlydir.pre-restore", "appendonlydir")); !os.IsNotExist(statErr) {
		t.Error("old AOF dir was nested into the stale pre-restore dir instead of replacing it")
	}
}

func TestInitScript_NoRestorePayload_KeepsAOFEnabled(t *testing.T) {
	conf, out, dataDir, err := runRestoreInitScript(t, "test-redis-0", masterEnv(), nil)
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}
	if got := lastAppendonlyDirective(conf); got != "yes" {
		t.Errorf("effective appendonly directive = %q, want yes on a normal boot", got)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "appendonlydir.pre-restore")); !os.IsNotExist(statErr) {
		t.Error("a normal boot must not touch the AOF dir")
	}
}
