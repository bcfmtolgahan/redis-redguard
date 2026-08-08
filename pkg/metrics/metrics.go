package metrics

import (
	"math"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// RedisClusterInfo tracks Redis cluster information. The master address is
	// deliberately not a label: it changes on every failover and is a pod IP,
	// so it would multiply the series of every cluster without bound. It is
	// reported on the CR instead, in status.masterNode.
	RedisClusterInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_cluster_info",
			Help: "Redis cluster basic information (1=up, 0=down)",
		},
		[]string{"namespace", "name"},
	)

	// RedisConnectedReplicas tracks number of connected replicas
	RedisConnectedReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_connected_replicas",
			Help: "Number of connected Redis replicas",
		},
		[]string{"namespace", "name"},
	)

	// RedisReplicationLag tracks replication lag in seconds
	RedisReplicationLag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_replication_lag_seconds",
			Help: "Redis replication lag in seconds (-1 when the replica could not be queried)",
		},
		[]string{"namespace", "name", "pod"},
	)

	// SentinelStatus tracks Sentinel health status
	SentinelStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_sentinel_status",
			Help: "Redis Sentinel status (1=healthy, 0=unhealthy)",
		},
		[]string{"namespace", "name"},
	)

	// SentinelMonitoredMasters tracks number of monitored masters
	SentinelMonitoredMasters = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_sentinel_monitored_masters",
			Help: "Number of masters monitored by Sentinel",
		},
		[]string{"namespace", "name"},
	)

	// RedisFailoverTotal tracks total number of failovers
	RedisFailoverTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "redis_failover_total",
			Help: "Total number of Redis failovers",
		},
		[]string{"namespace", "name"},
	)

	// RedisBackupStatus tracks backup status
	RedisBackupStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_backup_status",
			Help: "Redis backup status (1=success, 0=failed, -1=pending)",
		},
		[]string{"namespace", "name", "cluster"},
	)

	// RedisBackupDuration tracks backup duration in seconds
	RedisBackupDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "redis_backup_duration_seconds",
			Help:    "Redis backup duration in seconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10), // 1s to ~1024s
		},
		[]string{"namespace", "name"},
	)

	// RedisBackupSize tracks backup size in bytes
	RedisBackupSize = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_backup_size_bytes",
			Help: "Redis backup size in bytes",
		},
		[]string{"namespace", "name"},
	)

	// RedisUserACLStatus tracks ACL user status
	RedisUserACLStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_user_acl_status",
			Help: "Redis ACL user status (1=applied, 0=error)",
		},
		[]string{"namespace", "name", "username"},
	)

	// Reconcile duration and error counts are not declared here.
	// controller-runtime already exports controller_runtime_reconcile_time_seconds
	// and controller_runtime_reconcile_errors_total, labelled by controller,
	// for every controller registered with the Manager.

	// --- Memory Metrics ---

	// RedisUsedMemory tracks used memory in bytes
	RedisUsedMemory = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_used_memory_bytes",
			Help: "Redis used memory in bytes",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisMaxMemory tracks max memory setting in bytes
	RedisMaxMemory = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_max_memory_bytes",
			Help: "Redis max memory setting in bytes (0 means unlimited)",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisMemoryFragmentationRatio tracks memory fragmentation ratio
	RedisMemoryFragmentationRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_memory_fragmentation_ratio",
			Help: "Redis memory fragmentation ratio (used_memory_rss / used_memory)",
		},
		[]string{"namespace", "name", "pod"},
	)

	// --- Connection Metrics ---

	// RedisConnectedClients tracks number of connected clients
	RedisConnectedClients = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_connected_clients",
			Help: "Number of clients connected to Redis",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisBlockedClients tracks number of blocked clients
	RedisBlockedClients = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_blocked_clients",
			Help: "Number of clients blocked waiting for BLPOP, BRPOP, BRPOPLPUSH, etc.",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisRejectedConnections tracks total rejected connections
	RedisRejectedConnections = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "redis_rejected_connections_total",
			Help: "Total number of connections rejected because of maxclients limit",
		},
		[]string{"namespace", "name", "pod"},
	)

	// --- Persistence Metrics ---

	// RedisRDBLastSaveTime tracks last RDB save timestamp
	RedisRDBLastSaveTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_rdb_last_save_timestamp_seconds",
			Help: "Unix timestamp of the last successful RDB save",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisRDBChangesSinceLastSave tracks changes since last save
	RedisRDBChangesSinceLastSave = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_rdb_changes_since_last_save",
			Help: "Number of changes since last RDB save",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisAOFEnabled tracks if AOF is enabled
	RedisAOFEnabled = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_aof_enabled",
			Help: "Is AOF enabled (1=yes, 0=no)",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisAOFCurrentSize tracks current AOF file size
	RedisAOFCurrentSize = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_aof_current_size_bytes",
			Help: "Current AOF file size in bytes",
		},
		[]string{"namespace", "name", "pod"},
	)

	// --- Replication Metrics ---

	// RedisReplicationOffset tracks replication offset
	RedisReplicationOffset = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_replication_offset",
			Help: "Redis replication offset in bytes",
		},
		[]string{"namespace", "name", "pod", "role"},
	)

	// RedisReplicationLagBytes tracks replication lag in bytes (offset-based)
	RedisReplicationLagBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_replication_lag_bytes",
			Help: "Redis replication lag in bytes (master_repl_offset - slave_repl_offset)",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisMasterLinkStatus tracks master link status for replicas
	RedisMasterLinkStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_master_link_status",
			Help: "Redis replica master link status (1=up, 0=down)",
		},
		[]string{"namespace", "name", "pod"},
	)

	// --- Sentinel Metrics ---

	// SentinelQuorumHealth tracks if sentinel quorum is healthy
	SentinelQuorumHealth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_sentinel_quorum_healthy",
			Help: "Is Sentinel quorum healthy (1=yes, 0=no)",
		},
		[]string{"namespace", "name"},
	)

	// SentinelKnownSentinels tracks number of known sentinels
	SentinelKnownSentinels = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_sentinel_known_sentinels",
			Help: "Number of other known Sentinels for the master",
		},
		[]string{"namespace", "name"},
	)

	// SentinelKnownReplicas tracks number of known replicas
	SentinelKnownReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_sentinel_known_replicas",
			Help: "Number of known replicas for the master",
		},
		[]string{"namespace", "name"},
	)

	// --- Stats Metrics ---

	// RedisTotalCommandsProcessed tracks total commands processed
	RedisTotalCommandsProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "redis_total_commands_processed",
			Help: "Total number of commands processed by Redis",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisKeyspaceHits tracks keyspace hits
	RedisKeyspaceHits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "redis_keyspace_hits_total",
			Help: "Total number of successful key lookups",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisKeyspaceMisses tracks keyspace misses
	RedisKeyspaceMisses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "redis_keyspace_misses_total",
			Help: "Total number of failed key lookups",
		},
		[]string{"namespace", "name", "pod"},
	)

	// RedisDBKeys tracks number of keys per database
	RedisDBKeys = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_db_keys",
			Help: "Number of keys in the database",
		},
		[]string{"namespace", "name", "pod", "db"},
	)
)

func init() {
	// Register custom metrics with the controller-runtime metrics registry
	metrics.Registry.MustRegister(
		// Cluster info
		RedisClusterInfo,
		RedisConnectedReplicas,
		RedisReplicationLag,

		// Sentinel
		SentinelStatus,
		SentinelMonitoredMasters,
		SentinelQuorumHealth,
		SentinelKnownSentinels,
		SentinelKnownReplicas,

		// Failover
		RedisFailoverTotal,

		// Backup
		RedisBackupStatus,
		RedisBackupDuration,
		RedisBackupSize,

		// ACL
		RedisUserACLStatus,

		// Memory
		RedisUsedMemory,
		RedisMaxMemory,
		RedisMemoryFragmentationRatio,

		// Connections
		RedisConnectedClients,
		RedisBlockedClients,
		RedisRejectedConnections,

		// Persistence
		RedisRDBLastSaveTime,
		RedisRDBChangesSinceLastSave,
		RedisAOFEnabled,
		RedisAOFCurrentSize,

		// Replication
		RedisReplicationOffset,
		RedisReplicationLagBytes,
		RedisMasterLinkStatus,

		// Stats
		RedisTotalCommandsProcessed,
		RedisKeyspaceHits,
		RedisKeyspaceMisses,
		RedisDBKeys,
	)
}

// labelSep joins label values into a tracking key. No Kubernetes name or pod
// name can contain it, so two different label sets can never collide on one
// key.
const labelSep = "\x00"

// deletableVec is the shared surface of every *Vec type: they all embed
// *prometheus.MetricVec.
type deletableVec interface {
	prometheus.Collector
	DeletePartialMatch(prometheus.Labels) int
}

// clusterSeries are the vectors keyed by the namespace and name of a
// RedisSentinel. Backup and ACL vectors are not here: their "name" is the
// RedisBackup or RedisUser, not the cluster.
var clusterSeries = []deletableVec{
	RedisClusterInfo,
	RedisConnectedReplicas,
	RedisReplicationLag,
	SentinelStatus,
	SentinelMonitoredMasters,
	SentinelQuorumHealth,
	SentinelKnownSentinels,
	SentinelKnownReplicas,
	RedisFailoverTotal,
	RedisUsedMemory,
	RedisMaxMemory,
	RedisMemoryFragmentationRatio,
	RedisConnectedClients,
	RedisBlockedClients,
	RedisRejectedConnections,
	RedisRDBLastSaveTime,
	RedisRDBChangesSinceLastSave,
	RedisAOFEnabled,
	RedisAOFCurrentSize,
	RedisReplicationOffset,
	RedisReplicationLagBytes,
	RedisMasterLinkStatus,
	RedisTotalCommandsProcessed,
	RedisKeyspaceHits,
	RedisKeyspaceMisses,
	RedisDBKeys,
}

// podSeries are the cluster vectors carrying a per-pod label, so they outlive
// the pod itself unless they are dropped when it goes away.
var podSeries = []deletableVec{
	RedisReplicationLag,
	RedisUsedMemory,
	RedisMaxMemory,
	RedisMemoryFragmentationRatio,
	RedisConnectedClients,
	RedisBlockedClients,
	RedisRejectedConnections,
	RedisRDBLastSaveTime,
	RedisRDBChangesSinceLastSave,
	RedisAOFEnabled,
	RedisAOFCurrentSize,
	RedisReplicationOffset,
	RedisReplicationLagBytes,
	RedisMasterLinkStatus,
	RedisTotalCommandsProcessed,
	RedisKeyspaceHits,
	RedisKeyspaceMisses,
	RedisDBKeys,
}

// state holds what the collectors themselves cannot: the previous sample of
// each absolute Redis INFO counter, and the pods a cluster last reported.
var state = struct {
	mu       sync.Mutex
	counters map[*prometheus.CounterVec]map[string]float64
	pods     map[string]map[string]bool
}{
	counters: map[*prometheus.CounterVec]map[string]float64{},
	pods:     map[string]map[string]bool{},
}

// AddCounterDelta advances a counter by the increase since the previous sample
// of an absolute value read from Redis INFO.
//
// Those values are monotonic only within one process lifetime: a restarted
// Redis starts them again at zero. A sample below the previous one is
// therefore a restart, and the whole new value is the increase; subtracting
// would produce a negative Add, which Prometheus rejects. The first sample of
// a series only sets the baseline, so restarting the operator does not look
// like a burst of millions of commands to rate() and increase().
func AddCounterDelta(vec *prometheus.CounterVec, value float64, labelValues ...string) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}

	key := strings.Join(labelValues, labelSep)

	state.mu.Lock()
	series, ok := state.counters[vec]
	if !ok {
		series = map[string]float64{}
		state.counters[vec] = series
	}
	previous, seen := series[key]
	series[key] = value
	state.mu.Unlock()

	delta := 0.0
	switch {
	case !seen:
		// Baseline only.
	case value >= previous:
		delta = value - previous
	default:
		delta = value
	}
	vec.WithLabelValues(labelValues...).Add(delta)
}

// DeleteClusterSeries drops every series a RedisSentinel owns. Without it a
// deleted cluster keeps reporting itself up, and every alert written against
// it keeps evaluating against a value nothing updates any more.
func DeleteClusterSeries(namespace, name string) {
	labels := prometheus.Labels{"namespace": namespace, "name": name}
	for _, vec := range clusterSeries {
		vec.DeletePartialMatch(labels)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.pods, namespace+labelSep+name)
	forgetCounterBaselines(namespace, name, "")
}

// DeleteBackupSeries drops the series of one RedisBackup.
func DeleteBackupSeries(namespace, name string) {
	labels := prometheus.Labels{"namespace": namespace, "name": name}
	RedisBackupStatus.DeletePartialMatch(labels)
	RedisBackupDuration.DeletePartialMatch(labels)
	RedisBackupSize.DeletePartialMatch(labels)
}

// DeleteUserSeries drops the series of one RedisUser, including any left
// behind by an earlier spec.username.
func DeleteUserSeries(namespace, name string) {
	RedisUserACLStatus.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
}

// SyncClusterPods drops the per-pod series of pods the cluster no longer has.
// A scale-down otherwise leaves the removed pods reporting their last sample
// for as long as the operator runs.
func SyncClusterPods(namespace, name string, live []string) {
	cluster := namespace + labelSep + name
	current := make(map[string]bool, len(live))
	for _, pod := range live {
		current[pod] = true
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	for pod := range state.pods[cluster] {
		if current[pod] {
			continue
		}
		labels := prometheus.Labels{"namespace": namespace, "name": name, "pod": pod}
		for _, vec := range podSeries {
			vec.DeletePartialMatch(labels)
		}
		forgetCounterBaselines(namespace, name, pod)
	}

	state.pods[cluster] = current
}

// forgetCounterBaselines drops the tracked samples for a cluster, or for one
// of its pods when pod is set. The series are gone, so the next sample has to
// baseline again rather than be charged as a delta against a value nothing is
// counting from. Callers hold state.mu.
func forgetCounterBaselines(namespace, name, pod string) {
	prefix := namespace + labelSep + name
	if pod != "" {
		prefix += labelSep + pod
	}
	for _, series := range state.counters {
		for key := range series {
			if key == prefix || strings.HasPrefix(key, prefix+labelSep) {
				delete(series, key)
			}
		}
	}
}
