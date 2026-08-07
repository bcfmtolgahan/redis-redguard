package builder

import (
	"fmt"
	"strings"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildRedisConfigMap creates a ConfigMap for Redis configuration
func BuildRedisConfigMap(rs *redisv1alpha1.RedisSentinel) *corev1.ConfigMap {
	config := []string{
		"# Redis configuration",
		"bind 0.0.0.0",
		"port 6379",
		"dir /data",
		"appendonly yes",
		"appendfilename \"appendonly.aof\"",
		"save 900 1",
		"save 300 10",
		"save 60 10000",
	}

	// Add custom config
	for key, value := range rs.Spec.RedisConfig.CustomConfig {
		config = append(config, fmt.Sprintf("%s %s", key, value))
	}

	// Add auth if configured
	if rs.Spec.RedisConfig.Auth != nil && rs.Spec.RedisConfig.Auth.SecretName != "" {
		config = append(config, "masterauth ${REDIS_PASSWORD}")
		config = append(config, "requirepass ${REDIS_PASSWORD}")
	}

	// Add TLS configuration if enabled
	if rs.Spec.TLS != nil && rs.Spec.TLS.Enabled {
		// Disable non-TLS port and enable TLS port
		config = append(config,
			"",
			"# TLS Configuration",
			"port 0",
			"tls-port 6379",
			"tls-cert-file /etc/redis/tls/tls.crt",
			"tls-key-file /etc/redis/tls/tls.key",
			"tls-replication yes",
			"tls-cluster yes",
		)

		// Add CA certificate if specified
		if rs.Spec.TLS.CASecretRef != "" {
			config = append(config, "tls-ca-cert-file /etc/redis/tls/ca.crt")
		}

		// Configure mutual TLS if enabled
		if rs.Spec.TLS.MutualTLS {
			config = append(config, "tls-auth-clients yes")
		} else {
			config = append(config, "tls-auth-clients no")
		}
	}

	// Build sentinel hosts list for init script
	sentinelHosts := make([]string, rs.Spec.SentinelConfig.Replicas)
	for i := int32(0); i < rs.Spec.SentinelConfig.Replicas; i++ {
		sentinelHosts[i] = fmt.Sprintf("%s-sentinel-%d.%s-sentinel-headless.%s.svc.cluster.local",
			rs.Name, i, rs.Name, rs.Namespace)
	}

	masterName := rs.Name + "-master"
	bootstrapMasterHost := fmt.Sprintf("%s-redis-0.%s-redis-headless.%s.svc.cluster.local",
		rs.Name, rs.Name, rs.Namespace)

	initScript := buildRedisInitScript(sentinelHosts, masterName, bootstrapMasterHost)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-redis-config",
			Namespace: rs.Namespace,
			Labels:    buildLabels(rs, "redis"),
		},
		Data: map[string]string{
			"redis.conf": strings.Join(config, "\n"),
			"init.sh":    initScript,
		},
	}
}

// buildRedisInitScript creates the init script that queries Sentinel for master
func buildRedisInitScript(sentinelHosts []string, masterName, bootstrapMasterHost string) string {
	sentinelHostsStr := strings.Join(sentinelHosts, ",")

	return fmt.Sprintf(`#!/bin/sh
set -e

echo "=== Redis Init Script Starting ==="

# Function to safely escape password for sed replacement
# Escapes: / \ & and newlines
escape_for_sed() {
    printf '%%s' "$1" | sed -e 's/[\/&]/\\&/g' -e ':a' -e 'N' -e '$!ba' -e 's/\n/\\n/g'
}

# Copy config to writable location first
cp /etc/redis/redis.conf /data/redis.conf

# Replace password placeholder if auth is enabled
# Using awk for safer substitution (handles special characters better than sed)
if [ -n "$REDIS_PASSWORD" ]; then
    awk -v pass="$REDIS_PASSWORD" '{gsub(/\${REDIS_PASSWORD}/, pass); print}' /data/redis.conf > /data/redis.conf.tmp
    mv /data/redis.conf.tmp /data/redis.conf
fi

# Configuration
SENTINEL_HOSTS="%s"
MASTER_NAME="%s"
BOOTSTRAP_MASTER_HOST="%s"
POD_ORDINAL=${HOSTNAME##*-}
MY_IP=$(hostname -i)

echo "Pod ordinal: $POD_ORDINAL"
echo "My IP: $MY_IP"
echo "Sentinel hosts: $SENTINEL_HOSTS"
echo "Master name: $MASTER_NAME"

# Function to query sentinel for current master
# Uses REDISCLI_AUTH env var to avoid exposing password in process list
get_master_from_sentinel() {
    for sentinel in $(echo $SENTINEL_HOSTS | tr ',' ' '); do
        echo "Trying sentinel: $sentinel"
        # Query sentinel for master address using REDISCLI_AUTH for auth
        MASTER_INFO=$(REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli -h $sentinel -p 26379 SENTINEL get-master-addr-by-name $MASTER_NAME 2>/dev/null || true)
        if [ -n "$MASTER_INFO" ]; then
            # MASTER_INFO contains IP on first line, port on second
            MASTER_IP=$(echo "$MASTER_INFO" | head -1)
            if [ -n "$MASTER_IP" ] && [ "$MASTER_IP" != "nil" ]; then
                echo "Found master from sentinel: $MASTER_IP"
                echo "$MASTER_IP"
                return 0
            fi
        fi
    done
    return 1
}

# Function to check if an IP is myself
is_myself() {
    TARGET_IP=$1
    if [ "$TARGET_IP" = "$MY_IP" ]; then
        return 0
    fi
    # Also check if it resolves to my hostname
    MY_HOSTNAME=$(hostname -f 2>/dev/null || hostname)
    TARGET_HOSTNAME=$(getent hosts $TARGET_IP 2>/dev/null | awk '{print $2}' || true)
    if [ ! -z "$TARGET_HOSTNAME" ] && [ "$TARGET_HOSTNAME" = "$MY_HOSTNAME" ]; then
        return 0
    fi
    return 1
}

# Wait for sentinels to be available (with timeout)
echo "Waiting for sentinels to be ready..."
SENTINEL_READY=false
CURRENT_MASTER=""

for i in $(seq 1 30); do
    CURRENT_MASTER=$(get_master_from_sentinel)
    if [ ! -z "$CURRENT_MASTER" ]; then
        SENTINEL_READY=true
        echo "Sentinel is ready, current master: $CURRENT_MASTER"
        break
    fi
    echo "Waiting for sentinel... attempt $i/30"
    sleep 2
done

if [ "$SENTINEL_READY" = "true" ]; then
    # Sentinel is available - follow its master designation
    echo "Sentinel reports master at: $CURRENT_MASTER"

    if is_myself "$CURRENT_MASTER"; then
        echo "I am the current master (confirmed by Sentinel)"
        # Ensure we're not configured as a replica
        sed -i '/^replicaof/d' /data/redis.conf
        sed -i '/^slaveof/d' /data/redis.conf
    else
        echo "Configuring as replica of $CURRENT_MASTER"
        # Remove any existing replicaof config
        sed -i '/^replicaof/d' /data/redis.conf
        sed -i '/^slaveof/d' /data/redis.conf
        # Add replicaof directive
        echo "" >> /data/redis.conf
        echo "replicaof $CURRENT_MASTER 6379" >> /data/redis.conf
    fi
else
    # Bootstrap mode - sentinels not ready yet
    # This happens during initial cluster creation
    echo "Sentinel not ready - entering bootstrap mode"

    if [ "$POD_ORDINAL" = "0" ]; then
        echo "Starting as initial master (bootstrap mode)"
        # Ensure we're not configured as a replica
        sed -i '/^replicaof/d' /data/redis.conf
        sed -i '/^slaveof/d' /data/redis.conf
    else
        echo "Starting as replica of bootstrap master (bootstrap mode)"
        # Remove any existing replicaof config
        sed -i '/^replicaof/d' /data/redis.conf
        sed -i '/^slaveof/d' /data/redis.conf
        # Configure to replicate from pod-0
        echo "" >> /data/redis.conf
        echo "replicaof $BOOTSTRAP_MASTER_HOST 6379" >> /data/redis.conf
    fi
fi

echo "=== Final redis.conf ==="
grep -E "^(replicaof|slaveof|bind|port)" /data/redis.conf || true
echo "========================"

echo "Starting Redis server..."
exec redis-server /data/redis.conf
`, sentinelHostsStr, masterName, bootstrapMasterHost)
}

// BuildSentinelConfigMap creates a ConfigMap for Sentinel configuration
func BuildSentinelConfigMap(rs *redisv1alpha1.RedisSentinel) *corev1.ConfigMap {
	masterName := rs.Name + "-master"
	masterHost := fmt.Sprintf("%s-redis-0.%s-redis-headless.%s.svc.cluster.local",
		rs.Name, rs.Name, rs.Namespace)

	config := []string{
		"# Sentinel configuration",
		"bind 0.0.0.0",
		"port 26379",
		"dir /tmp",
		fmt.Sprintf("sentinel monitor %s %s 6379 %d", masterName, masterHost, rs.Spec.SentinelConfig.Quorum),
		fmt.Sprintf("sentinel down-after-milliseconds %s %d", masterName, rs.Spec.SentinelConfig.DownAfterMilliseconds),
		fmt.Sprintf("sentinel failover-timeout %s %d", masterName, rs.Spec.SentinelConfig.FailoverTimeout),
		fmt.Sprintf("sentinel parallel-syncs %s %d", masterName, rs.Spec.SentinelConfig.ParallelSyncs),
	}

	// Add auth if configured
	if rs.Spec.RedisConfig.Auth != nil && rs.Spec.RedisConfig.Auth.SecretName != "" {
		config = append(config, fmt.Sprintf("sentinel auth-pass %s ${REDIS_PASSWORD}", masterName))
	}

	// Add TLS configuration if enabled
	if rs.Spec.TLS != nil && rs.Spec.TLS.Enabled {
		config = append(config,
			"",
			"# TLS Configuration",
			"port 0",
			"tls-port 26379",
			"tls-cert-file /etc/sentinel/tls/tls.crt",
			"tls-key-file /etc/sentinel/tls/tls.key",
			"tls-replication yes",
		)

		// Add CA certificate if specified
		if rs.Spec.TLS.CASecretRef != "" {
			config = append(config, "tls-ca-cert-file /etc/sentinel/tls/ca.crt")
		}

		// Configure mutual TLS if enabled
		if rs.Spec.TLS.MutualTLS {
			config = append(config, "tls-auth-clients yes")
		} else {
			config = append(config, "tls-auth-clients no")
		}
	}

	// Add custom config
	for key, value := range rs.Spec.SentinelConfig.CustomConfig {
		config = append(config, fmt.Sprintf("%s %s", key, value))
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-sentinel-config",
			Namespace: rs.Namespace,
			Labels:    buildLabels(rs, "sentinel"),
		},
		Data: map[string]string{
			"sentinel.conf": strings.Join(config, "\n"),
			"init.sh": `#!/bin/sh
set -e

# Copy config to writable location first
cp /etc/sentinel/sentinel.conf /tmp/sentinel.conf

# Replace password placeholder if auth is enabled
# Using awk for safer substitution (handles special characters better than sed)
if [ -n "$REDIS_PASSWORD" ]; then
    awk -v pass="$REDIS_PASSWORD" '{gsub(/\${REDIS_PASSWORD}/, pass); print}' /tmp/sentinel.conf > /tmp/sentinel.conf.tmp
    mv /tmp/sentinel.conf.tmp /tmp/sentinel.conf
fi

# Wait for master to be available and get its IP
MASTER_HOST="` + masterHost + `"
echo "Waiting for master at $MASTER_HOST"
for i in $(seq 1 30); do
    MASTER_IP=$(getent hosts $MASTER_HOST | awk '{ print $1 }')
    if [ -n "$MASTER_IP" ]; then
        echo "Master resolved to IP: $MASTER_IP"
        # Replace hostname with IP in config using awk (safer than sed for special chars)
        awk -v host="$MASTER_HOST" -v ip="$MASTER_IP" '{gsub(host, ip); print}' /tmp/sentinel.conf > /tmp/sentinel.conf.tmp
        mv /tmp/sentinel.conf.tmp /tmp/sentinel.conf
        break
    fi
    echo "Waiting for master... ($i/30)"
    sleep 2
done

# Sentinel needs writable config
chmod 666 /tmp/sentinel.conf

exec redis-sentinel /tmp/sentinel.conf
`,
		},
	}
}

// buildLabels creates standard labels for resources
func buildLabels(rs *redisv1alpha1.RedisSentinel, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "redguard",
		"app.kubernetes.io/instance":   rs.Name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "redguard-operator",
	}
}
