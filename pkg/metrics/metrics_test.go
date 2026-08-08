package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// resetClusterMetrics clears the cluster vectors and the sampling state that
// tracks them, so specs cannot see each other's series.
func resetClusterMetrics() {
	for _, vec := range clusterSeries {
		if r, ok := vec.(interface{ Reset() }); ok {
			r.Reset()
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.counters = map[*prometheus.CounterVec]map[string]float64{}
	state.pods = map[string]map[string]bool{}
}

func newTestCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_counter_total", Help: "test"},
		[]string{"namespace", "name", "pod"},
	)
}

// The first sample only establishes a baseline. Adding it in full would make
// every operator restart look like a burst of millions of commands to
// increase() and rate().
func TestAddCounterDeltaBaselinesTheFirstSample(t *testing.T) {
	vec := newTestCounter()
	AddCounterDelta(vec, 5000, "ns", "cluster", "pod-0")

	if got := testutil.ToFloat64(vec.WithLabelValues("ns", "cluster", "pod-0")); got != 0 {
		t.Fatalf("first sample: got %v, want 0", got)
	}
}

func TestAddCounterDeltaAddsTheIncrease(t *testing.T) {
	vec := newTestCounter()
	AddCounterDelta(vec, 100, "ns", "cluster", "pod-0")
	AddCounterDelta(vec, 130, "ns", "cluster", "pod-0")
	AddCounterDelta(vec, 131, "ns", "cluster", "pod-0")

	if got := testutil.ToFloat64(vec.WithLabelValues("ns", "cluster", "pod-0")); got != 31 {
		t.Fatalf("got %v, want 31", got)
	}
}

// Redis INFO counters restart at zero when the node restarts. A sample below
// the previous one is that restart, not a negative delta: Prometheus panics on
// a negative Add.
func TestAddCounterDeltaTreatsADropAsARestart(t *testing.T) {
	vec := newTestCounter()
	AddCounterDelta(vec, 1000, "ns", "cluster", "pod-0")
	AddCounterDelta(vec, 1200, "ns", "cluster", "pod-0")
	AddCounterDelta(vec, 7, "ns", "cluster", "pod-0")

	if got := testutil.ToFloat64(vec.WithLabelValues("ns", "cluster", "pod-0")); got != 207 {
		t.Fatalf("got %v, want 207 (200 before the restart plus 7 after)", got)
	}
}

func TestAddCounterDeltaKeepsPodsIndependent(t *testing.T) {
	vec := newTestCounter()
	AddCounterDelta(vec, 100, "ns", "cluster", "pod-0")
	AddCounterDelta(vec, 900, "ns", "cluster", "pod-1")
	AddCounterDelta(vec, 150, "ns", "cluster", "pod-0")

	if got := testutil.ToFloat64(vec.WithLabelValues("ns", "cluster", "pod-0")); got != 50 {
		t.Fatalf("pod-0: got %v, want 50", got)
	}
	if got := testutil.ToFloat64(vec.WithLabelValues("ns", "cluster", "pod-1")); got != 0 {
		t.Fatalf("pod-1: got %v, want 0", got)
	}
}

func TestDeleteClusterSeriesRemovesOnlyThatCluster(t *testing.T) {
	resetClusterMetrics()
	t.Cleanup(resetClusterMetrics)

	RedisClusterInfo.WithLabelValues("ns", "gone").Set(1)
	RedisConnectedReplicas.WithLabelValues("ns", "gone").Set(2)
	SentinelQuorumHealth.WithLabelValues("ns", "gone").Set(1)
	RedisUsedMemory.WithLabelValues("ns", "gone", "gone-redis-0").Set(1024)
	RedisFailoverTotal.WithLabelValues("ns", "gone").Inc()

	RedisClusterInfo.WithLabelValues("ns", "kept").Set(1)
	RedisUsedMemory.WithLabelValues("ns", "kept", "kept-redis-0").Set(2048)

	DeleteClusterSeries("ns", "gone")

	for _, c := range []struct {
		name string
		vec  prometheus.Collector
	}{
		{"redis_cluster_info", RedisClusterInfo},
		{"redis_connected_replicas", RedisConnectedReplicas},
		{"redis_sentinel_quorum_healthy", SentinelQuorumHealth},
		{"redis_used_memory_bytes", RedisUsedMemory},
		{"redis_failover_total", RedisFailoverTotal},
	} {
		got := testutil.CollectAndCount(c.vec)
		want := 0
		if c.name == "redis_cluster_info" || c.name == "redis_used_memory_bytes" {
			want = 1
		}
		if got != want {
			t.Errorf("%s: %d series left, want %d", c.name, got, want)
		}
	}
}

// After the series are dropped the counter restarts at zero, so the tracked
// baseline has to go with them; otherwise the next sample is charged as a
// delta against a value nothing is counting from any more.
func TestDeleteClusterSeriesForgetsCounterBaselines(t *testing.T) {
	resetClusterMetrics()
	t.Cleanup(resetClusterMetrics)

	AddCounterDelta(RedisKeyspaceHits, 500, "ns", "gone", "gone-redis-0")
	AddCounterDelta(RedisKeyspaceHits, 900, "ns", "gone", "gone-redis-0")

	DeleteClusterSeries("ns", "gone")

	AddCounterDelta(RedisKeyspaceHits, 950, "ns", "gone", "gone-redis-0")
	if got := testutil.ToFloat64(RedisKeyspaceHits.WithLabelValues("ns", "gone", "gone-redis-0")); got != 0 {
		t.Fatalf("got %v, want 0: the sample after a delete must re-baseline", got)
	}
}

func TestSyncClusterPodsDropsSeriesForPodsThatAreGone(t *testing.T) {
	resetClusterMetrics()
	t.Cleanup(resetClusterMetrics)

	for _, pod := range []string{"c-redis-0", "c-redis-1", "c-redis-2"} {
		RedisUsedMemory.WithLabelValues("ns", "c", pod).Set(1)
		RedisMasterLinkStatus.WithLabelValues("ns", "c", pod).Set(1)
	}
	SyncClusterPods("ns", "c", []string{"c-redis-0", "c-redis-1", "c-redis-2"})

	SyncClusterPods("ns", "c", []string{"c-redis-0", "c-redis-1"})

	if got := testutil.CollectAndCount(RedisUsedMemory); got != 2 {
		t.Errorf("redis_used_memory_bytes: %d series left, want 2", got)
	}
	if got := testutil.CollectAndCount(RedisMasterLinkStatus); got != 2 {
		t.Errorf("redis_master_link_status: %d series left, want 2", got)
	}
}

func TestDeleteBackupSeriesRemovesTheBackupsOwnSeries(t *testing.T) {
	RedisBackupStatus.Reset()
	RedisBackupSize.Reset()
	t.Cleanup(func() {
		RedisBackupStatus.Reset()
		RedisBackupSize.Reset()
	})

	RedisBackupStatus.WithLabelValues("ns", "nightly", "cluster").Set(1)
	RedisBackupSize.WithLabelValues("ns", "nightly").Set(4096)
	RedisBackupStatus.WithLabelValues("ns", "weekly", "cluster").Set(1)

	DeleteBackupSeries("ns", "nightly")

	if got := testutil.CollectAndCount(RedisBackupStatus); got != 1 {
		t.Errorf("redis_backup_status: %d series left, want 1", got)
	}
	if got := testutil.CollectAndCount(RedisBackupSize); got != 0 {
		t.Errorf("redis_backup_size_bytes: %d series left, want 0", got)
	}
}

func TestDeleteUserSeriesRemovesEveryUsernameOfThatCR(t *testing.T) {
	RedisUserACLStatus.Reset()
	t.Cleanup(RedisUserACLStatus.Reset)

	RedisUserACLStatus.WithLabelValues("ns", "app-user", "app").Set(1)
	RedisUserACLStatus.WithLabelValues("ns", "app-user", "app-renamed").Set(0)
	RedisUserACLStatus.WithLabelValues("ns", "other-user", "other").Set(1)

	DeleteUserSeries("ns", "app-user")

	if got := testutil.CollectAndCount(RedisUserACLStatus); got != 1 {
		t.Errorf("redis_user_acl_status: %d series left, want 1", got)
	}
}
