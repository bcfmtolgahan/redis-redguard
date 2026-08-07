package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// seedRedisConf mirrors the shape of the generated redis.conf the init script
// copies from /etc/redis: base directives plus the auth placeholders.
const seedRedisConf = `bind 0.0.0.0
port 6379
dir /data
masterauth ${REDIS_PASSWORD}
requirepass ${REDIS_PASSWORD}
`

// runInitScript syntax-checks the generated script, rewrites its absolute
// paths into a temp dir, replaces the final exec and the retry sleep, and runs
// it under sh with the stub binaries from testdata/bin first on PATH. It
// returns the resulting redis.conf ("" when the script died before writing
// it), the combined stdout+stderr, and the script's error, if any.
func runInitScript(t *testing.T, script, hostname string, env map[string]string) (conf, output string, err error) {
	t.Helper()

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	etcDir := filepath.Join(dir, "etc")
	for _, d := range []string{dataDir, etcDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(etcDir, "redis.conf"), []byte(seedRedisConf), 0o644); err != nil {
		t.Fatal(err)
	}

	// Syntax-check the pristine script exactly as it ships.
	pristine := filepath.Join(dir, "init-pristine.sh")
	if err := os.WriteFile(pristine, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", pristine).CombinedOutput(); err != nil {
		t.Fatalf("generated script fails sh -n: %v\n%s", err, out)
	}

	// The script hardcodes /etc/redis and /data; point those at the temp dirs,
	// stub the final exec, and drop the retry delay so 30 attempts stay fast.
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
	// A minimal environment keeps the run hermetic: nothing secret-shaped can
	// leak in from the developer's shell.
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
		return "", string(out), runErr
	}
	return string(b), string(out), runErr
}

func testInitScript() string {
	return buildRedisInitScript(
		[]string{"s0.example.svc", "s1.example.svc"},
		"test-master",
		"test-redis-0.example.svc",
	)
}

func TestInitScript_SentinelAnswers_WritesCleanReplicaof(t *testing.T) {
	conf, out, err := runInitScript(t, testInitScript(), "test-redis-1",
		map[string]string{"REDGUARD_TEST_MASTER_IP": "10.244.0.5"})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(conf, "replicaof 10.244.0.5 6379") {
		t.Errorf("expected clean replicaof line, got config:\n%s", conf)
	}
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(line, "replicaof") && len(strings.Fields(line)) != 3 {
			t.Errorf("malformed replicaof (redis-server would fatal): %q", line)
		}
	}
	for _, leak := range []string{"Trying sentinel", "Found master", "querying sentinel", "[redguard-init]"} {
		if strings.Contains(conf, leak) {
			t.Errorf("log output leaked into config (%q):\n%s", leak, conf)
		}
	}
}

func TestInitScript_SentinelSaysThisPodIsMaster_NoReplicaof(t *testing.T) {
	conf, out, err := runInitScript(t, testInitScript(), "test-redis-1", map[string]string{
		"REDGUARD_TEST_MASTER_IP": "10.244.0.9",
		"REDGUARD_TEST_MY_IP":     "10.244.0.9",
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(conf, "replicaof") {
		t.Errorf("pod confirmed as master must not replicate, got:\n%s", conf)
	}
}

func TestInitScript_NoSentinel_Ordinal0_BootstrapsAsMaster(t *testing.T) {
	// No REDGUARD_TEST_MASTER_IP, so the stub redis-cli always fails: the
	// "sentinels not up yet" case on a fresh cluster.
	conf, out, err := runInitScript(t, testInitScript(), "test-redis-0", nil)
	if err != nil {
		t.Fatalf("bootstrap branch must be reachable, script failed: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(conf, "replicaof") {
		t.Errorf("ordinal 0 must start as master, got:\n%s", conf)
	}
	if !strings.Contains(out, "WOULD_EXEC redis-server") {
		t.Errorf("script must reach the exec, output:\n%s", out)
	}
}

func TestInitScript_NoSentinel_Ordinal1_FollowsBootstrapMaster(t *testing.T) {
	conf, out, err := runInitScript(t, testInitScript(), "test-redis-1", nil)
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(conf, "replicaof test-redis-0.example.svc 6379") {
		t.Errorf("ordinal 1 must follow bootstrap master, got:\n%s", conf)
	}
	if !strings.Contains(out, "WOULD_EXEC redis-server") {
		t.Errorf("script must reach the exec, output:\n%s", out)
	}
}

func TestInitScript_GarbageFromSentinel_IsRejected(t *testing.T) {
	conf, out, err := runInitScript(t, testInitScript(), "test-redis-0",
		map[string]string{"REDGUARD_TEST_MASTER_IP": "not-an-ip"})
	if err != nil {
		t.Fatalf("script must survive garbage input: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(conf, "not-an-ip") {
		t.Errorf("non-IP sentinel reply must never reach the config:\n%s", conf)
	}
}

// ---------------------------------------------------------------------------
// sentinel init script
// ---------------------------------------------------------------------------

// sentinelInitResult is what one execution of the sentinel init script left
// behind: the state file content and mode ("" / 0 when it does not exist),
// the combined output, the script error, and the temp data dir.
type sentinelInitResult struct {
	conf    string
	mode    os.FileMode
	output  string
	err     error
	dataDir string
}

// runSentinelInitScript syntax-checks the generated sentinel init script,
// rewrites its absolute paths into a temp dir, replaces the final exec and the
// retry sleep, seeds the ConfigMap-side sentinel.conf, optionally plants
// pre-existing state (a pod restart), and runs it under sh with the stub
// binaries from testdata/bin first on PATH.
func runSentinelInitScript(t *testing.T, script, seedConf, existingState string, env map[string]string) sentinelInitResult {
	t.Helper()

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	etcDir := filepath.Join(dir, "etc")
	for _, d := range []string{dataDir, etcDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(etcDir, "sentinel.conf"), []byte(seedConf), 0o644); err != nil {
		t.Fatal(err)
	}
	if existingState != "" {
		if err := os.WriteFile(filepath.Join(dataDir, "sentinel.conf"), []byte(existingState), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	pristine := filepath.Join(dir, "init-pristine.sh")
	if err := os.WriteFile(pristine, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", pristine).CombinedOutput(); err != nil {
		t.Fatalf("generated script fails sh -n: %v\n%s", err, out)
	}

	s := strings.ReplaceAll(script, "/etc/sentinel", etcDir)
	s = strings.ReplaceAll(s, "/data", dataDir)
	s = strings.ReplaceAll(s, "exec redis-sentinel", "echo WOULD_EXEC redis-sentinel")
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
	cmd.Env = []string{"PATH=" + stub + ":" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, runErr := cmd.CombinedOutput()

	res := sentinelInitResult{output: string(out), err: runErr, dataDir: dataDir}
	statePath := filepath.Join(dataDir, "sentinel.conf")
	if b, readErr := os.ReadFile(statePath); readErr == nil {
		res.conf = string(b)
	}
	if fi, statErr := os.Stat(statePath); statErr == nil {
		res.mode = fi.Mode()
	}
	return res
}

// testSentinelInit returns the generated sentinel init script and the
// sentinel.conf template it seeds from, with auth enabled.
func testSentinelInit(t *testing.T) (script, seedConf string) {
	t.Helper()
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}
	cm := BuildSentinelConfigMap(rs)
	return cm.Data["init.sh"], cm.Data["sentinel.conf"]
}

const sentinelMasterFQDN = "test-rs-redis-0.test-rs-redis-headless.default.svc.cluster.local"

func TestSentinelInitScript_FirstStart_SeedsSubstitutesResolves(t *testing.T) {
	// & is special in awk gsub replacements and \ starts escapes in awk -v
	// assignments; both must survive verbatim.
	const password = `sw&rd\fi&sh\\x`

	script, seedConf := testSentinelInit(t)
	res := runSentinelInitScript(t, script, seedConf, "", map[string]string{
		"REDGUARD_TEST_RESOLVE_IP": "10.244.0.7",
		"REDIS_PASSWORD":           password,
	})
	if res.err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", res.err, res.output)
	}
	if want := "sentinel monitor test-rs-master 10.244.0.7 6379 2"; !strings.Contains(res.conf, want) {
		t.Errorf("monitor target not resolved to the master IP, want %q in:\n%s", want, res.conf)
	}
	if strings.Contains(res.conf, sentinelMasterFQDN) {
		t.Errorf("master hostname left unresolved in state:\n%s", res.conf)
	}
	if want := "sentinel auth-pass test-rs-master " + password; !strings.Contains(res.conf, want) {
		t.Errorf("password not substituted literally, want %q in:\n%s", want, res.conf)
	}
	if strings.Contains(res.conf, "${REDIS_PASSWORD}") {
		t.Errorf("placeholder left behind:\n%s", res.conf)
	}
	if strings.Contains(res.output, password) {
		t.Errorf("password printed to stdout/stderr:\n%s", res.output)
	}
	if got := res.mode.Perm(); got != 0o600 {
		t.Errorf("state file mode = %o, want 600; it holds the redis password", got)
	}
	if want := "WOULD_EXEC redis-sentinel " + filepath.Join(res.dataDir, "sentinel.conf"); !strings.Contains(res.output, want) {
		t.Errorf("script must exec sentinel on the durable state file, output:\n%s", res.output)
	}
}

func TestSentinelInitScript_Restart_KeepsExistingState(t *testing.T) {
	// State a sentinel rewrote after a failover: the master is no longer the
	// bootstrap pod and the password is already literal. Re-seeding would
	// re-monitor redis-0; re-substituting would corrupt content that happens
	// to contain the placeholder text.
	existing := "port 26379\n" +
		"dir /data\n" +
		"sentinel monitor test-rs-master 10.9.9.9 6379 2\n" +
		"sentinel known-replica test-rs-master 10.9.9.8 6379\n" +
		"sentinel auth-pass test-rs-master literal${REDIS_PASSWORD}chunk\n" +
		"sentinel current-epoch 5\n"

	script, seedConf := testSentinelInit(t)
	// No REDGUARD_TEST_RESOLVE_IP: a restart must not depend on DNS at all.
	res := runSentinelInitScript(t, script, seedConf, existing, map[string]string{
		"REDIS_PASSWORD": "newpass",
	})
	if res.err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", res.err, res.output)
	}
	if res.conf != existing {
		t.Errorf("existing state was modified on restart:\ngot:\n%s\nwant:\n%s", res.conf, existing)
	}
	if want := "WOULD_EXEC redis-sentinel " + filepath.Join(res.dataDir, "sentinel.conf"); !strings.Contains(res.output, want) {
		t.Errorf("script must exec sentinel on the durable state file, output:\n%s", res.output)
	}
}

func TestSentinelInitScript_UnresolvableMaster_IsFatal(t *testing.T) {
	script, seedConf := testSentinelInit(t)
	// No REDGUARD_TEST_RESOLVE_IP: DNS never answers within the retry budget.
	res := runSentinelInitScript(t, script, seedConf, "", nil)

	if res.err == nil {
		t.Fatalf("script must exit non-zero when the master never resolves, output:\n%s", res.output)
	}
	if strings.Contains(res.output, "WOULD_EXEC") {
		t.Errorf("script must not start sentinel with an unresolvable monitor target, output:\n%s", res.output)
	}
	// A failed seed must leave no state file: the next start has to retry the
	// seed instead of keeping the unresolved hostname forever.
	if res.conf != "" {
		t.Errorf("failed seed left state behind, which a restart would then keep:\n%s", res.conf)
	}
}

func TestInitScript_PasswordSubstitution_SpecialCharsStayLiteral(t *testing.T) {
	// & is special in awk gsub replacements and \ starts escapes in awk -v
	// assignments; both must survive verbatim.
	const password = `sw&rd\fi&sh\\x`

	conf, out, err := runInitScript(t, testInitScript(), "test-redis-1", map[string]string{
		"REDGUARD_TEST_MASTER_IP": "10.244.0.5",
		"REDIS_PASSWORD":          password,
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(conf, "requirepass "+password) {
		t.Errorf("password not substituted literally, got config:\n%s", conf)
	}
	if strings.Contains(conf, "${REDIS_PASSWORD}") {
		t.Errorf("placeholder left behind:\n%s", conf)
	}
	if strings.Contains(out, password) {
		t.Errorf("password printed to stdout/stderr:\n%s", out)
	}
}
