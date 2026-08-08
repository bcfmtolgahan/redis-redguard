package builder

import (
	"regexp"
	"testing"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// podConfigHash reads the annotation the StatefulSet builders stamp on the pod
// template. It fails the test rather than returning an empty string: an absent
// annotation means a config change never reaches a running pod, which is the
// bug this file guards.
func podConfigHash(t *testing.T, annotations map[string]string) string {
	t.Helper()

	hash, ok := annotations[ConfigHashAnnotation]
	if !ok {
		t.Fatalf("pod template carries no %s annotation; a config change would never restart the pods, annotations=%v",
			ConfigHashAnnotation, annotations)
	}
	if !hexDigest.MatchString(hash) {
		t.Fatalf("%s = %q, want a sha256 hex digest", ConfigHashAnnotation, hash)
	}
	return hash
}

func TestPodTemplatesCarryAConfigHash(t *testing.T) {
	rs := testSentinel()

	redis := podConfigHash(t, BuildRedisStatefulSet(rs).Spec.Template.Annotations)
	sentinel := podConfigHash(t, BuildSentinelStatefulSet(rs).Spec.Template.Annotations)

	if redis == sentinel {
		t.Error("the Redis and Sentinel pod templates fingerprint different ConfigMaps and must not collide")
	}
}

// TestConfigHashIsStableAcrossRenders is the guard against a spurious rollout.
// The renderer walks customConfig as a Go map, whose iteration order is
// randomized per range statement, so a hash taken over the rendered bytes
// verbatim differs between two reconciles of an unchanged spec and restarts
// every pod on the periodic pass.
func TestConfigHashIsStableAcrossRenders(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.CustomConfig = map[string]string{
		"maxmemory":              "256mb",
		"maxmemory-policy":       "allkeys-lru",
		"timeout":                "300",
		"tcp-keepalive":          "60",
		"repl-backlog-size":      "1mb",
		"maxmemory-samples":      "5",
		"latency-monitor-thresh": "100",
	}
	rs.Spec.SentinelConfig.CustomConfig = map[string]string{
		"sentinel deny-scripts-reconfig": "yes",
		"loglevel":                       "notice",
		"tcp-keepalive":                  "60",
	}

	redis := podConfigHash(t, BuildRedisStatefulSet(rs).Spec.Template.Annotations)
	sentinel := podConfigHash(t, BuildSentinelStatefulSet(rs).Spec.Template.Annotations)

	for i := 0; i < 200; i++ {
		if got := podConfigHash(t, BuildRedisStatefulSet(rs).Spec.Template.Annotations); got != redis {
			t.Fatalf("redis config hash changed on render %d without a spec change: %q then %q", i, redis, got)
		}
		if got := podConfigHash(t, BuildSentinelStatefulSet(rs).Spec.Template.Annotations); got != sentinel {
			t.Fatalf("sentinel config hash changed on render %d without a spec change: %q then %q", i, sentinel, got)
		}
	}
}

func TestConfigHashFollowsRedisCustomConfig(t *testing.T) {
	base := podConfigHash(t, BuildRedisStatefulSet(testSentinel()).Spec.Template.Annotations)

	added := testSentinel()
	added.Spec.RedisConfig.CustomConfig = map[string]string{"maxmemory-policy": "allkeys-lru"}
	withKey := podConfigHash(t, BuildRedisStatefulSet(added).Spec.Template.Annotations)
	if withKey == base {
		t.Fatal("adding a customConfig directive left the hash unchanged; the edit would never reach a running pod")
	}

	changed := testSentinel()
	changed.Spec.RedisConfig.CustomConfig = map[string]string{"maxmemory-policy": "volatile-ttl"}
	if got := podConfigHash(t, BuildRedisStatefulSet(changed).Spec.Template.Annotations); got == withKey {
		t.Fatal("changing a customConfig value left the hash unchanged")
	}
}

func TestConfigHashFollowsAuthAndTLS(t *testing.T) {
	base := podConfigHash(t, BuildRedisStatefulSet(testSentinel()).Spec.Template.Annotations)

	auth := testSentinel()
	auth.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}
	if got := podConfigHash(t, BuildRedisStatefulSet(auth).Spec.Template.Annotations); got == base {
		t.Error("enabling auth adds requirepass to redis.conf but left the hash unchanged")
	}

	tls := testSentinel()
	tls.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: true, CertificateSecretRef: "redis-tls"}
	if got := podConfigHash(t, BuildRedisStatefulSet(tls).Spec.Template.Annotations); got == base {
		t.Error("enabling TLS rewrites the listener config but left the hash unchanged")
	}
}

// TestSentinelConfigHashFollowsMonitorParameters covers the fields an operator
// is most likely to edit after the cluster is running.
func TestSentinelConfigHashFollowsMonitorParameters(t *testing.T) {
	base := podConfigHash(t, BuildSentinelStatefulSet(testSentinel()).Spec.Template.Annotations)

	for name, mutate := range map[string]func(rs *redisv1alpha1.RedisSentinel){
		"quorum":                  func(rs *redisv1alpha1.RedisSentinel) { rs.Spec.SentinelConfig.Quorum = 3 },
		"down-after-milliseconds": func(rs *redisv1alpha1.RedisSentinel) { rs.Spec.SentinelConfig.DownAfterMilliseconds = 9000 },
		"failover-timeout":        func(rs *redisv1alpha1.RedisSentinel) { rs.Spec.SentinelConfig.FailoverTimeout = 20000 },
		"parallel-syncs":          func(rs *redisv1alpha1.RedisSentinel) { rs.Spec.SentinelConfig.ParallelSyncs = 2 },
		"customConfig": func(rs *redisv1alpha1.RedisSentinel) {
			rs.Spec.SentinelConfig.CustomConfig = map[string]string{"loglevel": "debug"}
		},
	} {
		rs := testSentinel()
		mutate(rs)
		if got := podConfigHash(t, BuildSentinelStatefulSet(rs).Spec.Template.Annotations); got == base {
			t.Errorf("changing %s left the sentinel config hash unchanged", name)
		}
	}
}

// TestSentinelCustomConfigDoesNotRollRedisPods pins the other half of the
// trade-off: a fingerprint wide enough to catch every real change but narrow
// enough that editing one component does not restart the other.
func TestSentinelCustomConfigDoesNotRollRedisPods(t *testing.T) {
	base := podConfigHash(t, BuildRedisStatefulSet(testSentinel()).Spec.Template.Annotations)

	rs := testSentinel()
	rs.Spec.SentinelConfig.CustomConfig = map[string]string{"loglevel": "debug"}
	rs.Spec.SentinelConfig.Quorum = 3

	if got := podConfigHash(t, BuildRedisStatefulSet(rs).Spec.Template.Annotations); got != base {
		t.Error("a sentinel-only edit restarted the Redis pods, including the master")
	}
}

// TestConfigHashCanonicalizesConfigFilesOnly documents the canonicalization
// contract directly: directive order inside a rendered .conf file is an
// artifact of map iteration and must not roll the pods, while line order in a
// script is meaning and must.
func TestConfigHashCanonicalizesConfigFilesOnly(t *testing.T) {
	if a, b := configHash(map[string]string{"redis.conf": "maxmemory 256mb\ntimeout 300"}),
		configHash(map[string]string{"redis.conf": "timeout 300\nmaxmemory 256mb"}); a != b {
		t.Error("reordered directives in a config file produced a different hash; every reconcile would roll the pods")
	}
	if a, b := configHash(map[string]string{"init.sh": "cp a b\nexec redis-server"}),
		configHash(map[string]string{"init.sh": "exec redis-server\ncp a b"}); a == b {
		t.Error("reordered script lines produced the same hash; a rewritten init script would never reach a pod")
	}
	if a, b := configHash(map[string]string{"redis.conf": "timeout 300"}),
		configHash(map[string]string{"sentinel.conf": "timeout 300"}); a == b {
		t.Error("the key a value is stored under must be part of the fingerprint")
	}
}
