package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	_, conf, output, err = runInitScriptInDataDir(t, script, hostname, env, nil)
	return conf, output, err
}

// runInitScriptInDataDir is runInitScript with access to the data volume: seed
// prepares it before the script runs, and its path is returned so a test can
// inspect files other than redis.conf.
func runInitScriptInDataDir(t *testing.T, script, hostname string, env map[string]string, seed func(dataDir string)) (dataDir, conf, output string, err error) {
	t.Helper()

	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	etcDir := filepath.Join(dir, "etc")
	for _, d := range []string{dataDir, etcDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(etcDir, "redis.conf"), []byte(seedRedisConf), 0o644); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(dataDir)
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
		return dataDir, "", string(out), runErr
	}
	return dataDir, string(b), string(out), runErr
}

func testInitScript() string {
	return buildRedisInitScript(
		[]string{"s0.example.svc", "s1.example.svc"},
		"test-master",
		"test-redis-0.example.svc",
		"",
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

// tlsInitScript builds the init script through BuildRedisConfigMap so the
// wiring from spec.tls to the script is what gets tested, not the script
// builder in isolation.
func tlsInitScript(t *testing.T, withCA bool) string {
	t.Helper()
	rs := testSentinel()
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: true, CertificateSecretRef: "redis-tls"}
	if withCA {
		rs.Spec.TLS.CASecretRef = "redis-ca"
	}
	return BuildRedisConfigMap(rs).Data["init.sh"]
}

// TestInitScript_TLS_QueriesSentinelOverTLS: with spec.tls enabled the sentinel
// config is `port 0` plus `tls-port 26379`, so a plaintext query gets an I/O
// error on every attempt. The stub refuses non-TLS queries the same way; a
// script without client TLS flags falls back to the bootstrap topology here and
// never learns the real master.
func TestInitScript_TLS_QueriesSentinelOverTLS(t *testing.T) {
	conf, out, err := runInitScript(t, tlsInitScript(t, true), "test-redis-1", map[string]string{
		"REDGUARD_TEST_MASTER_IP":   "10.244.0.5",
		"REDGUARD_TEST_REQUIRE_TLS": "1",
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(conf, "replicaof 10.244.0.5 6379") {
		t.Errorf("TLS-only sentinel was never reached; the pod fell back to the bootstrap topology:\n%s", conf)
	}
}

// TestInitScript_TLSFlagsMatchMountedPaths pins the sentinel query to the exact
// certificate paths the StatefulSet mounts and to the probes' CA rule: --cacert
// only when a CA secret is configured, because pointing it at a file that is
// not mounted fails every query.
func TestInitScript_TLSFlagsMatchMountedPaths(t *testing.T) {
	withCA := tlsInitScript(t, true)
	for _, want := range []string{
		"--tls",
		"--cert /etc/redis/tls/tls.crt",
		"--key /etc/redis/tls/tls.key",
		"--cacert /etc/redis/tls/ca.crt",
	} {
		if !strings.Contains(withCA, want) {
			t.Errorf("TLS init script does not carry %q", want)
		}
	}

	if noCA := tlsInitScript(t, false); strings.Contains(noCA, "--cacert") {
		t.Errorf("no CA secret configured but the init script passes --cacert")
	}

	if plain := BuildRedisConfigMap(testSentinel()).Data["init.sh"]; strings.Contains(plain, "--tls") {
		t.Errorf("TLS disabled but the init script passes --tls")
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
	// bootstrap pod. Re-seeding would re-monitor redis-0 and lose the epoch.
	existing := "port 26379\n" +
		"dir /data\n" +
		"sentinel monitor test-rs-master 10.9.9.9 6379 2\n" +
		"sentinel known-replica test-rs-master 10.9.9.8 6379\n" +
		"sentinel current-epoch 5\n"

	script, seedConf := testSentinelInit(t)
	// No REDGUARD_TEST_RESOLVE_IP: a restart must not depend on DNS at all.
	res := runSentinelInitScript(t, script, seedConf, existing, map[string]string{
		"REDIS_PASSWORD": "newpass",
	})
	if res.err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", res.err, res.output)
	}
	for _, line := range strings.Split(strings.TrimRight(existing, "\n"), "\n") {
		if !strings.Contains(res.conf, line+"\n") {
			t.Errorf("restart dropped learned state line %q:\n%s", line, res.conf)
		}
	}
	if strings.Contains(res.conf, "test-rs-redis-0.") {
		t.Errorf("restart re-seeded the monitor target from the template:\n%s", res.conf)
	}
	if want := "WOULD_EXEC redis-sentinel " + filepath.Join(res.dataDir, "sentinel.conf"); !strings.Contains(res.output, want) {
		t.Errorf("script must exec sentinel on the durable state file, output:\n%s", res.output)
	}
}

// sentinelCredentialLines returns the state-file lines that carry a credential,
// in file order.
func sentinelCredentialLines(conf string) []string {
	var out []string
	for _, l := range strings.Split(conf, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "requirepass",
			f[0] == "sentinel" && len(f) > 1 && (f[1] == "auth-pass" || f[1] == "sentinel-pass"),
			f[0] == "user":
			out = append(out, l)
		}
	}
	return out
}

// TestSentinelInitScript_Restart_RebuildsCredentials covers the state file
// sentinel owns after the first start: it is kept verbatim across restarts, so
// a file written before requirepass existed would leave port 26379 open
// forever, and a rotated password would never reach sentinel.
func TestSentinelInitScript_Restart_RebuildsCredentials(t *testing.T) {
	const password = `sw&rd\fi&sh\\x`

	// A pre-auth state file plus the stale default-user ACL a config rewrite
	// leaves behind, which would otherwise outrank a later requirepass.
	existing := "port 26379\n" +
		"sentinel monitor test-rs-master 10.9.9.9 6379 2\n" +
		"sentinel auth-pass test-rs-master oldpassword\n" +
		"sentinel current-epoch 5\n" +
		"user default on #" + sha256Hex("oldpassword") + " ~* &* +@all\n"

	script, seedConf := testSentinelInit(t)
	res := runSentinelInitScript(t, script, seedConf, existing, map[string]string{
		"REDIS_PASSWORD": password,
	})
	if res.err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", res.err, res.output)
	}

	want := []string{
		"sentinel auth-pass test-rs-master " + password,
		"sentinel sentinel-pass " + password,
		"requirepass " + password,
	}
	if got := sentinelCredentialLines(res.conf); !slices.Equal(got, want) {
		t.Errorf("credential lines = %q, want exactly %q (in that order, after the monitor line):\n%s", got, want, res.conf)
	}
	if !strings.Contains(res.conf, "sentinel current-epoch 5") {
		t.Errorf("rebuilding credentials dropped learned state:\n%s", res.conf)
	}
	if strings.Contains(res.output, password) {
		t.Errorf("password printed to stdout/stderr:\n%s", res.output)
	}
	if got := res.mode.Perm(); got != 0o600 {
		t.Errorf("state file mode = %o, want 600; it holds the redis password", got)
	}
}

// TestSentinelInitScript_NoPassword_DropsCredentials is the reverse direction:
// clearing auth must not leave sentinel demanding a password nobody holds.
func TestSentinelInitScript_NoPassword_DropsCredentials(t *testing.T) {
	existing := "port 26379\n" +
		"sentinel monitor test-rs-master 10.9.9.9 6379 2\n" +
		"sentinel auth-pass test-rs-master oldpassword\n" +
		"sentinel sentinel-pass oldpassword\n" +
		"requirepass oldpassword\n" +
		"user default on #" + sha256Hex("oldpassword") + " ~* &* +@all\n"

	rs := testSentinel()
	cm := BuildSentinelConfigMap(rs)
	res := runSentinelInitScript(t, cm.Data["init.sh"], cm.Data["sentinel.conf"], existing, nil)
	if res.err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", res.err, res.output)
	}
	if got := sentinelCredentialLines(res.conf); len(got) != 0 {
		t.Errorf("auth disabled but the state file still carries credentials %q:\n%s", got, res.conf)
	}
	if !strings.Contains(res.conf, "sentinel monitor test-rs-master 10.9.9.9 6379 2") {
		t.Errorf("dropping credentials dropped learned state:\n%s", res.conf)
	}
}

func TestSentinelInitScript_FirstStart_WritesCredentials(t *testing.T) {
	const password = `sw&rd\fi&sh\\x`

	script, seedConf := testSentinelInit(t)
	res := runSentinelInitScript(t, script, seedConf, "", map[string]string{
		"REDGUARD_TEST_RESOLVE_IP": "10.244.0.7",
		"REDIS_PASSWORD":           password,
	})
	if res.err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", res.err, res.output)
	}
	want := []string{
		"sentinel auth-pass test-rs-master " + password,
		"sentinel sentinel-pass " + password,
		"requirepass " + password,
	}
	if got := sentinelCredentialLines(res.conf); !slices.Equal(got, want) {
		t.Errorf("credential lines = %q, want exactly %q:\n%s", got, want, res.conf)
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

// ---------------------------------------------------------------------------
// ACL file durability
// ---------------------------------------------------------------------------

// readACLFile returns the ACL file the init script prepared on the data volume,
// together with its permission bits.
func readACLFile(t *testing.T, dataDir string) (string, os.FileMode) {
	t.Helper()

	path := filepath.Join(dataDir, "users.acl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no ACL file on the data volume: %v (redis-server aborts at startup when aclfile is missing)", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), info.Mode().Perm()
}

// aclUserLines returns the ACL file lines defining the given user.
func aclUserLines(acl, username string) []string {
	var out []string
	for _, line := range strings.Split(acl, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "user" && fields[1] == username {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestInitScript_CreatesACLFileWithDefaultUser(t *testing.T) {
	const password = "adminpw"

	dataDir, _, out, err := runInitScriptInDataDir(t, testInitScript(), "test-redis-0",
		map[string]string{"REDIS_PASSWORD": password}, nil)
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	acl, mode := readACLFile(t, dataDir)

	// An ACL file without a 'user default' line resets the default account to
	// nopass, which silently discards requirepass and opens the instance.
	lines := aclUserLines(acl, "default")
	if len(lines) != 1 {
		t.Fatalf("want exactly one 'user default' line, got %d:\n%s", len(lines), acl)
	}
	want := "user default on #" + sha256Hex(password) + " ~* &* +@all"
	if lines[0] != want {
		t.Errorf("default user line = %q, want %q", lines[0], want)
	}
	if strings.Contains(acl, password) {
		t.Errorf("ACL file holds the plaintext password:\n%s", acl)
	}
	if strings.Contains(out, password) {
		t.Errorf("password printed to stdout/stderr:\n%s", out)
	}
	if mode != 0o600 {
		t.Errorf("ACL file mode = %04o, want 0600", mode)
	}
}

func TestInitScript_KeepsSavedACLUsersAndRebuildsDefault(t *testing.T) {
	const appLine = "user app on #71d3d4e36cf4bc4309fd7390ba79fc6a1d03d8d7b45ef26f04bcdff3ff116868 ~app:* resetchannels -@all +@read"
	const stale = "user default on #0000000000000000000000000000000000000000000000000000000000000000 ~* &* +@all"

	seed := func(dataDir string) {
		body := appLine + "\n" + stale + "\n"
		if err := os.WriteFile(filepath.Join(dataDir, "users.acl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dataDir, _, out, err := runInitScriptInDataDir(t, testInitScript(), "test-redis-1",
		map[string]string{"REDIS_PASSWORD": "rotated"}, seed)
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	acl, _ := readACLFile(t, dataDir)

	if got := aclUserLines(acl, "app"); len(got) != 1 || got[0] != appLine {
		t.Errorf("saved ACL user not preserved across restart, got %q:\n%s", got, acl)
	}
	// A rotated password must reach the default account, or the old one keeps
	// working and the operator cannot authenticate with the new one.
	lines := aclUserLines(acl, "default")
	if len(lines) != 1 {
		t.Fatalf("want exactly one 'user default' line, got %d:\n%s", len(lines), acl)
	}
	if want := "user default on #" + sha256Hex("rotated") + " ~* &* +@all"; lines[0] != want {
		t.Errorf("default user line = %q, want %q", lines[0], want)
	}
}

func TestInitScript_NoPasswordLeavesDefaultUserOpen(t *testing.T) {
	dataDir, _, out, err := runInitScriptInDataDir(t, testInitScript(), "test-redis-0", nil, nil)
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	acl, _ := readACLFile(t, dataDir)

	lines := aclUserLines(acl, "default")
	if len(lines) != 1 {
		t.Fatalf("want exactly one 'user default' line, got %d:\n%s", len(lines), acl)
	}
	if want := "user default on nopass ~* &* +@all"; lines[0] != want {
		t.Errorf("default user line = %q, want %q", lines[0], want)
	}
}
