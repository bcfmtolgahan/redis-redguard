package builder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// probeCLIStub stands in for redis-cli. It answers PING and INFO replication
// from the environment, refuses a password passed on the command line, and can
// assert that the probe delivered one through REDISCLI_AUTH instead.
const probeCLIStub = `#!/bin/sh
case " $* " in
*" -a "*)
    echo "password passed on the command line" >&2
    exit 64
    ;;
esac
if [ -n "${STUB_EXPECT_AUTH:-}" ] && [ "${REDISCLI_AUTH:-}" != "$STUB_EXPECT_AUTH" ]; then
    echo "NOAUTH Authentication required." >&2
    exit 1
fi
case "$*" in
*ping)
    [ -n "${STUB_PING:-}" ] || exit 1
    printf '%s\n' "$STUB_PING"
    ;;
*"info replication")
    [ -n "${STUB_INFO:-}" ] || exit 1
    printf '%s' "$STUB_INFO"
    ;;
*)
    echo "unexpected redis-cli args: $*" >&2
    exit 2
    ;;
esac
`

// replicationInfo renders an INFO replication payload the way Redis does, with
// CRLF line endings, so the probe's matcher is exercised on the real shape.
func replicationInfo(lines ...string) string {
	return "# Replication\r\n" + strings.Join(lines, "\r\n") + "\r\n"
}

// probeScript extracts the shell script out of an exec probe.
func probeScript(t *testing.T, p *corev1.Probe) string {
	t.Helper()
	if p == nil || p.Exec == nil {
		t.Fatal("probe has no exec handler")
	}
	cmd := p.Exec.Command
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("probe command is not `sh -c <script>`: %v", cmd)
	}
	return cmd[2]
}

func redisProbes(t *testing.T, rs *redisv1alpha1.RedisSentinel) (liveness, readiness string) {
	t.Helper()
	c := BuildRedisStatefulSet(rs).Spec.Template.Spec.Containers[0]
	return probeScript(t, c.LivenessProbe), probeScript(t, c.ReadinessProbe)
}

// runProbeScript runs a probe script under sh with the stub redis-cli first on
// PATH and reports whether it exited zero, which is the only signal kubelet
// takes from an exec probe.
func runProbeScript(t *testing.T, script string, env map[string]string) (ok bool, output string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "redis-cli"), []byte(probeCLIStub), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("probe script fails sh -n: %v\nscript: %s\n%s", err, script, out)
	}

	cmd := exec.Command("sh", "-c", script)
	// A minimal environment keeps the run hermetic: nothing secret-shaped can
	// leak in from the developer's shell.
	cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// TestReadinessProbeChecksReplicationState pins the split between the two
// probes. Liveness stays a bare ping so a replica with a broken link is removed
// from the Service rather than restarted; readiness additionally demands a
// replication state that can serve correct reads.
func TestReadinessProbeChecksReplicationState(t *testing.T) {
	liveness, readiness := redisProbes(t, testSentinel())

	if liveness == readiness {
		t.Fatalf("readiness and liveness run the same command, so an unsynced replica is served traffic: %q", readiness)
	}
	if strings.Contains(liveness, "info replication") {
		t.Errorf("liveness inspects replication state and would restart a replica with a broken link: %q", liveness)
	}
	if !strings.Contains(readiness, "info replication") {
		t.Errorf("readiness does not read INFO replication: %q", readiness)
	}
	for _, want := range []string{"PONG", "role:master", "master_link_status:up"} {
		if !strings.Contains(readiness, want) {
			t.Errorf("readiness does not check for %q: %q", want, readiness)
		}
	}
}

// TestReadinessProbeVerdicts runs the generated script under a real sh against
// every replication state a Redis pod reaches.
func TestReadinessProbeVerdicts(t *testing.T) {
	liveness, readiness := redisProbes(t, testSentinel())

	for _, tc := range []struct {
		name    string
		ping    string
		info    string
		ready   bool
		alive   bool
		because string
	}{
		{
			name:    "master",
			ping:    "PONG",
			info:    replicationInfo("role:master", "connected_slaves:2", "master_failover_state:no-failover"),
			ready:   true,
			alive:   true,
			because: "a master is always readable",
		},
		{
			name:    "replica with a live link",
			ping:    "PONG",
			info:    replicationInfo("role:slave", "master_host:10.0.0.1", "master_link_status:up", "master_sync_in_progress:0"),
			ready:   true,
			alive:   true,
			because: "a synced replica serves correct reads",
		},
		{
			name:    "replica with a broken link",
			ping:    "PONG",
			info:    replicationInfo("role:slave", "master_host:10.0.0.1", "master_link_status:down", "master_sync_in_progress:0"),
			ready:   false,
			alive:   true,
			because: "its data is stale, but restarting it would not help",
		},
		{
			name:    "replica still running its first sync",
			ping:    "PONG",
			info:    replicationInfo("role:slave", "master_host:10.0.0.1", "master_link_status:down", "master_sync_in_progress:1"),
			ready:   false,
			alive:   true,
			because: "it has no data yet and would answer reads as empty",
		},
		{
			name:  "server loading the dataset",
			ping:  "LOADING Redis is loading the dataset in memory",
			info:  replicationInfo("role:master", "connected_slaves:0"),
			ready: false,
			alive: false,
			// INFO answers during loading, so only the PING half rejects this.
			because: "it cannot serve any command yet",
		},
		{
			name:    "server unreachable",
			ready:   false,
			alive:   false,
			because: "nothing answers on the port",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"STUB_PING": tc.ping, "STUB_INFO": tc.info}

			if ok, out := runProbeScript(t, readiness, env); ok != tc.ready {
				t.Errorf("readiness = %v, want %v (%s)\noutput: %s", ok, tc.ready, tc.because, out)
			}
			if ok, out := runProbeScript(t, liveness, env); ok != tc.alive {
				t.Errorf("liveness = %v, want %v (%s)\noutput: %s", ok, tc.alive, tc.because, out)
			}
		})
	}
}

// TestReadinessProbeAuthenticatesEveryCall covers the failure mode of splitting
// one probe into two redis-cli calls: an auth prefix that only reaches the
// first leaves the second answering NOAUTH, and every pod stays unready.
func TestReadinessProbeAuthenticatesEveryCall(t *testing.T) {
	// A password with a space and a glob would break on any unquoted expansion
	// that is subject to field splitting.
	const password = "p@ss w*rd"

	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}
	liveness, readiness := redisProbes(t, rs)

	env := map[string]string{
		"REDIS_PASSWORD":   password,
		"STUB_EXPECT_AUTH": password,
		"STUB_PING":        "PONG",
		"STUB_INFO":        replicationInfo("role:slave", "master_link_status:up"),
	}
	for name, script := range map[string]string{"liveness": liveness, "readiness": readiness} {
		if ok, out := runProbeScript(t, script, env); !ok {
			t.Errorf("%s probe failed against a password-protected server: %s", name, out)
		}
		if strings.Contains(script, "-a ") || strings.Contains(script, "--pass") {
			t.Errorf("%s probe puts the password in argv, where any process in the pod can read it: %q", name, script)
		}
		if !strings.Contains(script, "REDISCLI_AUTH=$REDIS_PASSWORD") {
			t.Errorf("%s probe does not take the password from the environment: %q", name, script)
		}
	}

	// Without auth configured nothing must reference the variable.
	_, plain := redisProbes(t, testSentinel())
	if strings.Contains(plain, "REDISCLI_AUTH") {
		t.Errorf("auth disabled but readiness references REDISCLI_AUTH: %q", plain)
	}
}

// TestReadinessProbeTLSFlags mirrors the liveness rules: --cacert only when a CA
// secret is mounted, since pointing it at a missing file fails every tick.
func TestReadinessProbeTLSFlags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tls        *redisv1alpha1.TLSConfig
		wantTLS    bool
		wantCacert bool
	}{
		{name: "disabled"},
		{
			name:    "enabled without a CA secret",
			tls:     &redisv1alpha1.TLSConfig{Enabled: true, CertificateSecretRef: "redis-tls"},
			wantTLS: true,
		},
		{
			name:       "enabled with a CA secret",
			tls:        &redisv1alpha1.TLSConfig{Enabled: true, CertificateSecretRef: "redis-tls", CASecretRef: "redis-ca"},
			wantTLS:    true,
			wantCacert: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := testSentinel()
			rs.Spec.TLS = tc.tls
			_, readiness := redisProbes(t, rs)

			if got := strings.Contains(readiness, "--tls"); got != tc.wantTLS {
				t.Errorf("readiness --tls = %v, want %v: %q", got, tc.wantTLS, readiness)
			}
			if got := strings.Contains(readiness, "--cacert /etc/redis/tls/ca.crt"); got != tc.wantCacert {
				t.Errorf("readiness --cacert = %v, want %v: %q", got, tc.wantCacert, readiness)
			}
			// Both redis-cli calls have to carry the same client material.
			if n := strings.Count(readiness, "--tls"); tc.wantTLS && n != strings.Count(readiness, "redis-cli") {
				t.Errorf("%d of %d redis-cli calls carry --tls: %q", n, strings.Count(readiness, "redis-cli"), readiness)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Real container
// ---------------------------------------------------------------------------

// TestReadinessProbeAgainstRealRedis settles the parsing against redis:7-alpine
// itself: the stub can only assert what we already believe INFO looks like.
func TestReadinessProbeAgainstRealRedis(t *testing.T) {
	if testing.Short() {
		t.Skip("starts containers")
	}
	requireDockerImage(t, redisProbeImage)

	liveness, readiness := redisProbes(t, testSentinel())

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()%1e6)
	network := "redguard-probe-" + suffix
	dockerRun(t, 30*time.Second, "network", "create", network)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })

	master := "redguard-master-" + suffix
	linkUp := "redguard-replica-up-" + suffix
	linkDown := "redguard-replica-down-" + suffix

	// bind is set for the same reason the generated redis.conf sets it: without
	// it protected mode refuses every non-loopback client, probe included.
	startRedis(t, network, master, "--bind", "0.0.0.0", "--save", "", "--appendonly", "no")
	startRedis(t, network, linkUp, "--bind", "0.0.0.0", "--save", "", "--appendonly", "no",
		"--replicaof", master, "6379")
	// A master name that does not resolve leaves the link permanently down.
	startRedis(t, network, linkDown, "--bind", "0.0.0.0", "--save", "", "--appendonly", "no",
		"--replicaof", "no-such-master.invalid", "6379")

	waitForRedis(t, master)
	waitForRedis(t, linkUp)
	waitForRedis(t, linkDown)
	waitForLinkUp(t, linkUp)

	for _, tc := range []struct {
		container string
		ready     bool
	}{
		{master, true},
		{linkUp, true},
		{linkDown, false},
	} {
		ok, out := dockerExecScript(t, tc.container, readiness)
		if ok != tc.ready {
			t.Errorf("%s: readiness = %v, want %v\n%s\nINFO:\n%s",
				tc.container, ok, tc.ready, out, redisInfo(t, tc.container))
		}
		// Liveness must hold everywhere: a replica with a dead link is removed
		// from endpoints, never restarted.
		if ok, out := dockerExecScript(t, tc.container, liveness); !ok {
			t.Errorf("%s: liveness failed: %s", tc.container, out)
		}
	}
}

const redisProbeImage = "redis:7-alpine"

func requireDockerImage(t *testing.T, image string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("docker daemon is not reachable")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil {
		return
	}
	pull, cancelPull := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelPull()
	if out, err := exec.CommandContext(pull, "docker", "pull", image).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s: %v\n%s", image, err, out)
	}
}

func dockerRun(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func startRedis(t *testing.T, network, name string, serverArgs ...string) {
	t.Helper()
	args := []string{"run", "-d", "--name", name, "--hostname", name, "--network", network,
		redisProbeImage, "redis-server"}
	dockerRun(t, time.Minute, append(args, serverArgs...)...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
}

func dockerExecScript(t *testing.T, container, script string) (ok bool, output string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c", script).CombinedOutput()
	return err == nil, string(out)
}

func redisInfo(t *testing.T, container string) string {
	t.Helper()
	out, _ := exec.Command("docker", "exec", container, "redis-cli", "info", "replication").CombinedOutput()
	return string(out)
}

func waitForRedis(t *testing.T, container string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "exec", container, "redis-cli", "ping").CombinedOutput()
		if err == nil && strings.Contains(string(out), "PONG") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never answered PING", container)
}

func waitForLinkUp(t *testing.T, container string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(redisInfo(t, container), "master_link_status:up") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never reached master_link_status:up:\n%s", container, redisInfo(t, container))
}
