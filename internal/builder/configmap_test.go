package builder

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// testSentinel returns a minimal, valid RedisSentinel mirroring newTestSentinel
// from internal/controller/suite_test.go. Tests mutate the returned value freely.
func testSentinel() *redisv1alpha1.RedisSentinel {
	return &redisv1alpha1.RedisSentinel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rs",
			Namespace: "default",
		},
		Spec: redisv1alpha1.RedisSentinelSpec{
			RedisConfig: redisv1alpha1.RedisConfig{
				Replicas: 3,
				Image:    "redis:7-alpine",
				Storage: &redisv1alpha1.StorageSpec{
					Size: resource.MustParse("1Gi"),
				},
			},
			SentinelConfig: redisv1alpha1.SentinelConfig{
				Replicas:              3,
				Quorum:                2,
				DownAfterMilliseconds: 5000,
				FailoverTimeout:       10000,
				ParallelSyncs:         1,
			},
			ServiceType: corev1.ServiceTypeClusterIP,
		},
	}
}

// configLines splits a rendered config file into non-empty, non-comment lines.
func configLines(conf string) []string {
	var out []string
	for _, l := range strings.Split(conf, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// findDirective returns the first config line whose first field(s) match prefix.
func findDirective(conf, prefix string) (string, bool) {
	for _, l := range configLines(conf) {
		if strings.HasPrefix(l, prefix+" ") || l == prefix {
			return l, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// redis.conf
// ---------------------------------------------------------------------------

func TestBuildRedisConfigMap_Metadata(t *testing.T) {
	rs := testSentinel()
	cm := BuildRedisConfigMap(rs)

	if got, want := cm.Name, "test-rs-redis-config"; got != want {
		t.Errorf("ConfigMap name = %q, want %q", got, want)
	}
	if got, want := cm.Namespace, "default"; got != want {
		t.Errorf("ConfigMap namespace = %q, want %q", got, want)
	}
	for _, key := range []string{"redis.conf", "init.sh"} {
		if _, ok := cm.Data[key]; !ok {
			t.Errorf("ConfigMap is missing data key %q (has %v)", key, mapKeys(cm.Data))
		}
	}
	wantLabels := buildLabels(rs, "redis")
	for k, v := range wantLabels {
		if cm.Labels[k] != v {
			t.Errorf("ConfigMap label %s = %q, want %q", k, cm.Labels[k], v)
		}
	}
}

func TestBuildRedisConfigMap_BaseDirectives(t *testing.T) {
	conf := BuildRedisConfigMap(testSentinel()).Data["redis.conf"]

	want := []string{
		"bind 0.0.0.0",
		"port 6379",
		"dir /data",
		"appendonly yes",
	}
	for _, w := range want {
		if !strings.Contains(conf, w) {
			t.Errorf("redis.conf missing %q, got:\n%s", w, conf)
		}
	}
}

func TestBuildRedisConfigMap_DeclaresACLFile(t *testing.T) {
	conf := BuildRedisConfigMap(testSentinel()).Data["redis.conf"]

	// Without an aclfile every ACL SETUSER is runtime-only state: the init
	// script re-copies redis.conf from the ConfigMap on each start, so anything
	// CONFIG REWRITE persisted there is discarded on the next restart.
	line, ok := findDirective(conf, "aclfile")
	if !ok {
		t.Fatalf("redis.conf declares no aclfile, so ACL users vanish on restart:\n%s", conf)
	}
	if want := "aclfile /data/users.acl"; line != want {
		t.Errorf("aclfile = %q, want %q (the data volume is the only writable, durable path)", line, want)
	}
}

func TestBuildRedisConfigMap_AuthEnabled(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}

	conf := BuildRedisConfigMap(rs).Data["redis.conf"]

	for _, want := range []string{"requirepass ${REDIS_PASSWORD}", "masterauth ${REDIS_PASSWORD}"} {
		if !strings.Contains(conf, want) {
			t.Errorf("expected password placeholder %q, got:\n%s", want, conf)
		}
	}
}

func TestBuildRedisConfigMap_NoAuthHasNoRequirepass(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = nil

	conf := BuildRedisConfigMap(rs).Data["redis.conf"]

	for _, unwanted := range []string{"requirepass", "masterauth"} {
		if _, found := findDirective(conf, unwanted); found {
			t.Errorf("auth disabled but redis.conf contains %q:\n%s", unwanted, conf)
		}
	}
}

func TestBuildRedisConfigMap_EmptySecretNameIsNoAuth(t *testing.T) {
	rs := testSentinel()
	// An Auth block that names no secret must not enable auth: the operator would
	// have nothing to project into REDIS_PASSWORD.
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: ""}

	conf := BuildRedisConfigMap(rs).Data["redis.conf"]

	if _, found := findDirective(conf, "requirepass"); found {
		t.Errorf("empty secretName must not render requirepass:\n%s", conf)
	}
}

// TestConfigMapsNeverEmbedLiteralPassword is the security invariant: a password
// must only ever reach the config as the ${REDIS_PASSWORD} placeholder, resolved
// at runtime from a projected env var. Nothing secret-shaped may be baked in.
func TestConfigMapsNeverEmbedLiteralPassword(t *testing.T) {
	const secretName = "my-redis-credentials"

	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: secretName}

	cms := map[string]*corev1.ConfigMap{
		"redis":    BuildRedisConfigMap(rs),
		"sentinel": BuildSentinelConfigMap(rs),
	}

	// Every directive that carries a credential must carry exactly the placeholder.
	credentialDirectives := []string{"requirepass", "masterauth", "sentinel auth-pass", "sentinel sentinel-pass"}

	for kind, cm := range cms {
		for file, body := range cm.Data {
			if strings.Contains(body, secretName) {
				t.Errorf("%s ConfigMap %s embeds the Secret name %q; credentials must be referenced via env only:\n%s",
					kind, file, secretName, body)
			}
			for _, d := range credentialDirectives {
				line, found := findDirective(body, d)
				if !found {
					continue
				}
				fields := strings.Fields(line)
				last := fields[len(fields)-1]
				if last != "${REDIS_PASSWORD}" {
					t.Errorf("%s ConfigMap %s: %q ends with %q, want the ${REDIS_PASSWORD} placeholder",
						kind, file, line, last)
				}
			}
		}
	}
}

func TestBuildRedisConfigMap_CustomConfigRendered(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.CustomConfig = map[string]string{
		"maxmemory":        "256mb",
		"maxmemory-policy": "allkeys-lru",
	}

	conf := BuildRedisConfigMap(rs).Data["redis.conf"]

	for k, v := range rs.Spec.RedisConfig.CustomConfig {
		want := k + " " + v
		if !strings.Contains(conf, want) {
			t.Errorf("redis.conf missing custom config %q, got:\n%s", want, conf)
		}
	}
}

func TestBuildRedisConfigMap_TLSDisabledHasNoTLSDirectives(t *testing.T) {
	rs := testSentinel()
	rs.Spec.TLS = nil

	conf := BuildRedisConfigMap(rs).Data["redis.conf"]

	if strings.Contains(conf, "tls-") {
		t.Errorf("TLS disabled but redis.conf contains tls- directives:\n%s", conf)
	}
	if line, _ := findDirective(conf, "port"); line != "port 6379" {
		t.Errorf("TLS disabled: port line = %q, want %q", line, "port 6379")
	}
}

func TestBuildRedisConfigMap_TLSEnabled(t *testing.T) {
	tests := []struct {
		name        string
		tls         redisv1alpha1.TLSConfig
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name: "cert only",
			tls: redisv1alpha1.TLSConfig{
				Enabled:              true,
				CertificateSecretRef: "redis-tls",
			},
			wantPresent: []string{
				"port 0",
				"tls-port 6379",
				"tls-cert-file /etc/redis/tls/tls.crt",
				"tls-key-file /etc/redis/tls/tls.key",
				"tls-replication yes",
				"tls-auth-clients no",
			},
			wantAbsent: []string{"tls-ca-cert-file"},
		},
		{
			name: "with CA",
			tls: redisv1alpha1.TLSConfig{
				Enabled:              true,
				CertificateSecretRef: "redis-tls",
				CASecretRef:          "redis-ca",
			},
			wantPresent: []string{"tls-ca-cert-file /etc/redis/tls/ca.crt"},
		},
		{
			name: "mutual TLS",
			tls: redisv1alpha1.TLSConfig{
				Enabled:              true,
				CertificateSecretRef: "redis-tls",
				MutualTLS:            true,
			},
			wantPresent: []string{"tls-auth-clients yes"},
			wantAbsent:  []string{"tls-auth-clients no"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := testSentinel()
			tlsCfg := tc.tls
			rs.Spec.TLS = &tlsCfg

			conf := BuildRedisConfigMap(rs).Data["redis.conf"]

			for _, want := range tc.wantPresent {
				if !strings.Contains(conf, want) {
					t.Errorf("redis.conf missing %q, got:\n%s", want, conf)
				}
			}
			for _, unwanted := range tc.wantAbsent {
				if strings.Contains(conf, unwanted) {
					t.Errorf("redis.conf unexpectedly contains %q, got:\n%s", unwanted, conf)
				}
			}
		})
	}
}

func TestBuildRedisConfigMap_InitScriptKnowsAllSentinels(t *testing.T) {
	rs := testSentinel()
	rs.Spec.SentinelConfig.Replicas = 5

	script := BuildRedisConfigMap(rs).Data["init.sh"]

	for i := 0; i < 5; i++ {
		host := fmt.Sprintf("test-rs-sentinel-%d.test-rs-sentinel-headless.default.svc.cluster.local", i)
		if !strings.Contains(script, host) {
			t.Errorf("init.sh missing sentinel host %q, got:\n%s", host, script)
		}
	}
	if !strings.Contains(script, `MASTER_NAME="test-rs-master"`) {
		t.Errorf("init.sh does not set the expected MASTER_NAME, got:\n%s", script)
	}
	wantBootstrap := `BOOTSTRAP_MASTER_HOST="test-rs-redis-0.test-rs-redis-headless.default.svc.cluster.local"`
	if !strings.Contains(script, wantBootstrap) {
		t.Errorf("init.sh missing %q, got:\n%s", wantBootstrap, script)
	}
}

// ---------------------------------------------------------------------------
// sentinel.conf
// ---------------------------------------------------------------------------

func TestBuildSentinelConfigMap_Metadata(t *testing.T) {
	rs := testSentinel()
	cm := BuildSentinelConfigMap(rs)

	if got, want := cm.Name, "test-rs-sentinel-config"; got != want {
		t.Errorf("ConfigMap name = %q, want %q", got, want)
	}
	if got, want := cm.Namespace, "default"; got != want {
		t.Errorf("ConfigMap namespace = %q, want %q", got, want)
	}
	for _, key := range []string{"sentinel.conf", "init.sh"} {
		if _, ok := cm.Data[key]; !ok {
			t.Errorf("ConfigMap is missing data key %q (has %v)", key, mapKeys(cm.Data))
		}
	}
	wantLabels := buildLabels(rs, "sentinel")
	for k, v := range wantLabels {
		if cm.Labels[k] != v {
			t.Errorf("ConfigMap label %s = %q, want %q", k, cm.Labels[k], v)
		}
	}
}

func TestBuildSentinelConfigMap_QuorumRendered(t *testing.T) {
	rs := testSentinel()
	rs.Spec.SentinelConfig.Quorum = 2

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	line, found := findDirective(conf, "sentinel monitor")
	if !found {
		t.Fatalf("no `sentinel monitor` directive in:\n%s", conf)
	}
	want := "sentinel monitor test-rs-master test-rs-redis-0.test-rs-redis-headless.default.svc.cluster.local 6379 2"
	if line != want {
		t.Errorf("monitor line = %q, want %q", line, want)
	}
}

func TestBuildSentinelConfigMap_TimersRendered(t *testing.T) {
	rs := testSentinel()
	rs.Spec.SentinelConfig.Quorum = 3
	rs.Spec.SentinelConfig.DownAfterMilliseconds = 1234
	rs.Spec.SentinelConfig.FailoverTimeout = 45000
	rs.Spec.SentinelConfig.ParallelSyncs = 2

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	tests := []struct{ prefix, want string }{
		{"sentinel monitor", "sentinel monitor test-rs-master test-rs-redis-0.test-rs-redis-headless.default.svc.cluster.local 6379 3"},
		{"sentinel down-after-milliseconds", "sentinel down-after-milliseconds test-rs-master 1234"},
		{"sentinel failover-timeout", "sentinel failover-timeout test-rs-master 45000"},
		{"sentinel parallel-syncs", "sentinel parallel-syncs test-rs-master 2"},
	}
	for _, tc := range tests {
		line, found := findDirective(conf, tc.prefix)
		if !found {
			t.Errorf("missing directive %q in:\n%s", tc.prefix, conf)
			continue
		}
		if line != tc.want {
			t.Errorf("%s = %q, want %q", tc.prefix, line, tc.want)
		}
	}
}

func TestBuildSentinelConfigMap_BaseDirectives(t *testing.T) {
	conf := BuildSentinelConfigMap(testSentinel()).Data["sentinel.conf"]

	want := []string{
		"bind 0.0.0.0",
		"port 26379",
		"dir /data",
		"sentinel resolve-hostnames yes",
		"sentinel announce-hostnames no",
	}
	for _, w := range want {
		if !strings.Contains(conf, w) {
			t.Errorf("sentinel.conf missing %q, got:\n%s", w, conf)
		}
	}
	if strings.Contains(conf, "dir /tmp") {
		t.Errorf("sentinel state must not live in /tmp:\n%s", conf)
	}
	// Directives apply in order: sentinel rejects a hostname monitor target
	// unless resolve-hostnames is already enabled when the line is parsed.
	if r, m := strings.Index(conf, "sentinel resolve-hostnames yes"), strings.Index(conf, "sentinel monitor "); r == -1 || m == -1 || r > m {
		t.Errorf("resolve-hostnames must precede the monitor directive:\n%s", conf)
	}
}

func TestBuildSentinelConfigMap_AuthPass(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	want := "sentinel auth-pass test-rs-master ${REDIS_PASSWORD}"
	if !strings.Contains(conf, want) {
		t.Errorf("sentinel.conf missing %q, got:\n%s", want, conf)
	}
}

func TestBuildSentinelConfigMap_NoAuthHasNoAuthPass(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = nil

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	if strings.Contains(conf, "auth-pass") {
		t.Errorf("auth disabled but sentinel.conf contains auth-pass:\n%s", conf)
	}
}

// TestSentinelConfigRequiresPasswordWhenAuthEnabled covers the control plane
// itself: "sentinel auth-pass" is the credential sentinel presents to redis, not
// one clients must present. Without requirepass the default user on 26379 stays
// nopass and any pod that can reach the port may issue SENTINEL FAILOVER, SET or
// REMOVE.
func TestSentinelConfigRequiresPasswordWhenAuthEnabled(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	line, ok := findDirective(conf, "requirepass")
	if !ok {
		t.Fatalf("auth is configured but sentinel.conf has no requirepass, so port 26379 accepts any credential:\n%s", conf)
	}
	if want := "requirepass ${REDIS_PASSWORD}"; line != want {
		t.Errorf("requirepass = %q, want %q", line, want)
	}
}

// TestSentinelConfigAuthenticatesToPeers guards the other half of requirepass:
// sentinels PING each other and vote through SENTINEL is-master-down-by-addr on
// the same protected port, so without a peer credential every sentinel sees the
// others as down and no failover can be authorized.
func TestSentinelConfigAuthenticatesToPeers(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	line, ok := findDirective(conf, "sentinel sentinel-pass")
	if !ok {
		t.Fatalf("requirepass without sentinel-pass leaves peers unable to authenticate:\n%s", conf)
	}
	if want := "sentinel sentinel-pass ${REDIS_PASSWORD}"; line != want {
		t.Errorf("sentinel-pass = %q, want %q", line, want)
	}
}

func TestBuildSentinelConfigMap_NoAuthHasNoRequirepass(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = nil

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	for _, unwanted := range []string{"requirepass", "sentinel sentinel-pass"} {
		if _, found := findDirective(conf, unwanted); found {
			t.Errorf("auth disabled but sentinel.conf contains %q:\n%s", unwanted, conf)
		}
	}
}

func TestBuildSentinelConfigMap_EmptySecretNameIsNoAuth(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: ""}

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	for _, unwanted := range []string{"requirepass", "sentinel sentinel-pass"} {
		if _, found := findDirective(conf, unwanted); found {
			t.Errorf("empty secretName must not render %q:\n%s", unwanted, conf)
		}
	}
}

func TestBuildSentinelConfigMap_TLS(t *testing.T) {
	tests := []struct {
		name        string
		tls         redisv1alpha1.TLSConfig
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name: "disabled",
			tls:  redisv1alpha1.TLSConfig{Enabled: false},
			wantAbsent: []string{
				"tls-port", "tls-cert-file", "tls-key-file", "tls-auth-clients",
			},
		},
		{
			name: "enabled",
			tls: redisv1alpha1.TLSConfig{
				Enabled:              true,
				CertificateSecretRef: "sentinel-tls",
			},
			wantPresent: []string{
				"port 0",
				"tls-port 26379",
				"tls-cert-file /etc/sentinel/tls/tls.crt",
				"tls-key-file /etc/sentinel/tls/tls.key",
				"tls-replication yes",
				"tls-auth-clients no",
			},
			wantAbsent: []string{"tls-ca-cert-file"},
		},
		{
			name: "enabled with CA and mTLS",
			tls: redisv1alpha1.TLSConfig{
				Enabled:              true,
				CertificateSecretRef: "sentinel-tls",
				CASecretRef:          "sentinel-ca",
				MutualTLS:            true,
			},
			wantPresent: []string{
				"tls-ca-cert-file /etc/sentinel/tls/ca.crt",
				"tls-auth-clients yes",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := testSentinel()
			tlsCfg := tc.tls
			rs.Spec.TLS = &tlsCfg

			conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

			for _, want := range tc.wantPresent {
				if !strings.Contains(conf, want) {
					t.Errorf("sentinel.conf missing %q, got:\n%s", want, conf)
				}
			}
			for _, unwanted := range tc.wantAbsent {
				if strings.Contains(conf, unwanted) {
					t.Errorf("sentinel.conf unexpectedly contains %q, got:\n%s", unwanted, conf)
				}
			}
		})
	}
}

func TestBuildSentinelConfigMap_CustomConfigRendered(t *testing.T) {
	rs := testSentinel()
	rs.Spec.SentinelConfig.CustomConfig = map[string]string{
		"sentinel deny-scripts-reconfig": "yes",
	}

	conf := BuildSentinelConfigMap(rs).Data["sentinel.conf"]

	if !strings.Contains(conf, "sentinel deny-scripts-reconfig yes") {
		t.Errorf("sentinel.conf missing custom config, got:\n%s", conf)
	}
}

func TestBuildSentinelConfigMap_InitScriptResolvesMaster(t *testing.T) {
	script := BuildSentinelConfigMap(testSentinel()).Data["init.sh"]

	want := `MASTER_HOST="test-rs-redis-0.test-rs-redis-headless.default.svc.cluster.local"`
	if !strings.Contains(script, want) {
		t.Errorf("sentinel init.sh missing %q, got:\n%s", want, script)
	}
	if !strings.Contains(script, `exec redis-sentinel "$STATE"`) {
		t.Errorf("sentinel init.sh must exec redis-sentinel on the durable state file, got:\n%s", script)
	}
	if strings.Contains(script, "/tmp/sentinel.conf") {
		t.Errorf("sentinel state must not live in /tmp:\n%s", script)
	}
	if strings.Contains(script, "chmod 666") {
		t.Errorf("state file holds the redis password; it must not be world-readable:\n%s", script)
	}
}

// ---------------------------------------------------------------------------
// customConfig validation
// ---------------------------------------------------------------------------

func TestCustomConfigRejectsNewlines(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]string
	}{
		{"newline in value", map[string]string{"maxmemory": "1gb\nrequirepass hacked"}},
		{"carriage return in value", map[string]string{"maxmemory": "1gb\rrequirepass hacked"}},
		{"crlf in value", map[string]string{"maxmemory": "1gb\r\nrequirepass hacked"}},
		{"newline in key", map[string]string{"maxmemory 1gb\nrequirepass": "hacked"}},
		{"trailing newline in value", map[string]string{"maxmemory": "1gb\n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCustomConfig(tc.cfg); err == nil {
				t.Errorf("ValidateCustomConfig(%q) = nil, want line-break rejection", tc.cfg)
			}
		})
	}
}

func TestCustomConfigCannotOverrideAuthDirectives(t *testing.T) {
	reserved := []string{
		"requirepass",
		"REQUIREPASS",
		"Requirepass",
		"masterauth",
		"masteruser",
		"user",
		"aclfile",
		"tls-port",
		"tls-cert-file",
		"tls-auth-clients",
		"TLS-PORT",
		"port",
		"bind",
		"replicaof",
		"slaveof",
		"dir",
		"include",
		"loadmodule",
		"rename-command",
		"unixsocket",
		"protected-mode",
		"enable-protected-configs",
		"enable-debug-command",
		"enable-module-command",
		"replica-announce-ip",
		"sentinel monitor",
		"sentinel auth-pass",
		"sentinel auth-user",
		"sentinel announce-ip",
		"sentinel resolve-hostnames",
		"SENTINEL AUTH-PASS",
	}
	for _, key := range reserved {
		if err := ValidateCustomConfig(map[string]string{key: "x"}); err == nil {
			t.Errorf("ValidateCustomConfig accepted reserved key %q", key)
		}
	}
}

func TestCustomConfigRejectsMalformedKeys(t *testing.T) {
	malformed := []string{
		"",
		"   ",
		"maxmemory 1gb",
		"maxmemory-policy allkeys-lru extra",
		"sentinel",
		"sentinel deny-scripts-reconfig yes",
		"foo;bar",
		"föö",
	}
	for _, key := range malformed {
		if err := ValidateCustomConfig(map[string]string{key: "x"}); err == nil {
			t.Errorf("ValidateCustomConfig accepted malformed key %q", key)
		}
	}
}

func TestCustomConfigAllowsTuningDirectives(t *testing.T) {
	allowed := map[string]string{
		"maxmemory":                      "256mb",
		"maxmemory-policy":               "allkeys-lru",
		"appendfsync":                    "everysec",
		"save":                           "900 1",
		"client-output-buffer-limit":     "normal 0 0 0",
		"sentinel deny-scripts-reconfig": "yes",
	}
	for k, v := range allowed {
		if err := ValidateCustomConfig(map[string]string{k: v}); err != nil {
			t.Errorf("ValidateCustomConfig rejected legitimate entry %q=%q: %v", k, v, err)
		}
	}
}

// ---------------------------------------------------------------------------
// render determinism
// ---------------------------------------------------------------------------

// determinismSentinel returns a CR whose customConfig maps are large enough that
// an unordered renderer emits a different directive order on almost every call.
func determinismSentinel() *redisv1alpha1.RedisSentinel {
	rs := testSentinel()
	rs.Spec.RedisConfig.CustomConfig = map[string]string{
		"maxmemory":           "256mb",
		"maxmemory-policy":    "allkeys-lru",
		"timeout":             "300",
		"tcp-keepalive":       "60",
		"maxclients":          "1000",
		"lazyfree-lazy-evict": "yes",
	}
	rs.Spec.SentinelConfig.CustomConfig = map[string]string{
		"sentinel deny-scripts-reconfig": "yes",
		"sentinel notification-script":   "/dev/null",
		"maxclients":                     "1000",
		"tcp-keepalive":                  "60",
		"timeout":                        "0",
		"loglevel":                       "notice",
	}
	return rs
}

// TestConfigMapRenderingIsDeterministic pins the rendered config files against
// Go's randomized map iteration. The reconciler compares the freshly rendered
// Data against the live ConfigMap, so a render that merely reorders directives
// makes every pass issue an Update, and the Owns(&corev1.ConfigMap{}) watch
// turns that Update back into a reconcile: an unbounded write loop against the
// API server for any spec with two or more customConfig keys.
//
// Go re-randomizes the iteration start on every range statement, so with six
// keys an unordered renderer repeats the reference order with probability well
// under 1/2 per call. 200 renders therefore let a regression pass with
// probability below 2^-199: the count is a bound, not a guess. Two keys would
// give only a coin flip per render and make the test itself flaky.
func TestConfigMapRenderingIsDeterministic(t *testing.T) {
	rs := determinismSentinel()

	wantRedis := BuildRedisConfigMap(rs).Data
	wantSentinel := BuildSentinelConfigMap(rs).Data

	const renders = 200
	for i := range renders {
		assertSameData(t, i, "redis", wantRedis, BuildRedisConfigMap(rs).Data)
		assertSameData(t, i, "sentinel", wantSentinel, BuildSentinelConfigMap(rs).Data)
	}
}

// assertSameData fails on the first key whose rendered bytes drifted.
func assertSameData(t *testing.T, render int, component string, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s render %d produced %d keys, want %d", component, render, len(got), len(want))
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if got[key] != want[key] {
			t.Fatalf("%s render %d changed %s:\nfirst render:\n%s\nthis render:\n%s",
				component, render, key, want[key], got[key])
		}
	}
}

// TestCustomConfigRendersInSortedKeyOrder pins the order itself, so a future
// renderer cannot become deterministic by accident of map layout.
func TestCustomConfigRendersInSortedKeyOrder(t *testing.T) {
	rs := determinismSentinel()

	tests := []struct {
		name string
		conf string
		cfg  map[string]string
	}{
		{"redis", BuildRedisConfigMap(rs).Data["redis.conf"], rs.Spec.RedisConfig.CustomConfig},
		{"sentinel", BuildSentinelConfigMap(rs).Data["sentinel.conf"], rs.Spec.SentinelConfig.CustomConfig},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var want []string
			for _, key := range slices.Sorted(maps.Keys(tc.cfg)) {
				want = append(want, key+" "+tc.cfg[key])
			}

			lines := configLines(tc.conf)
			var got []string
			for _, l := range lines {
				if slices.Contains(want, l) {
					got = append(got, l)
				}
			}

			if !slices.Equal(got, want) {
				t.Errorf("custom directives rendered as %v, want %v\nfull config:\n%s", got, want, tc.conf)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------

func TestBuildLabels(t *testing.T) {
	rs := testSentinel()

	got := buildLabels(rs, "redis")
	want := map[string]string{
		"app.kubernetes.io/name":       "redguard",
		"app.kubernetes.io/instance":   "test-rs",
		"app.kubernetes.io/component":  "redis",
		"app.kubernetes.io/managed-by": "redguard-operator",
	}
	if len(got) != len(want) {
		t.Errorf("buildLabels returned %d labels, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %s = %q, want %q", k, got[k], v)
		}
	}

	if c := buildLabels(rs, "sentinel")["app.kubernetes.io/component"]; c != "sentinel" {
		t.Errorf("component label = %q, want %q", c, "sentinel")
	}
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
