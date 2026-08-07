package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// RedisClusterInfo tracks Redis cluster information
	RedisClusterInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_cluster_info",
			Help: "Redis cluster basic information (1=up, 0=down)",
		},
		[]string{"namespace", "name", "master_node"},
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
			Help: "Redis replication lag in seconds",
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

	// ReconciliationDuration tracks controller reconciliation duration
	ReconciliationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "redis_controller_reconciliation_duration_seconds",
			Help:    "Duration of controller reconciliation in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller", "namespace", "name"},
	)

	// ReconciliationErrors tracks reconciliation errors
	ReconciliationErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "redis_controller_reconciliation_errors_total",
			Help: "Total number of reconciliation errors",
		},
		[]string{"controller", "namespace", "name", "error_type"},
	)

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

		// Controller
		ReconciliationDuration,
		ReconciliationErrors,

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
