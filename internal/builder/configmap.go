package builder

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
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

// buildRedisInitScript creates the init script that queries Sentinel for master.
// Invariants the script must hold: only the master IP is ever written on stdout
// inside command substitution (all progress goes to stderr), a sentinel reply is
// validated as an IPv4 address before it can reach redis.conf, no command may
// abort the retry loop under set -e, and the password never appears in output
// or in a process argument list.
func buildRedisInitScript(sentinelHosts []string, masterName, bootstrapMasterHost string) string {
	sentinelHostsStr := strings.Join(sentinelHosts, ",")

	return fmt.Sprintf(`#!/bin/sh
set -eu

log() { echo "[redguard-init] $*" >&2; }

SENTINEL_HOSTS="%s"
MASTER_NAME="%s"
BOOTSTRAP_MASTER_HOST="%s"
POD_ORDINAL=${HOSTNAME##*-}
MY_IP=$(hostname -i 2>/dev/null | awk '{print $1}')

log "pod ordinal=$POD_ORDINAL ip=$MY_IP master-name=$MASTER_NAME"

cp /etc/redis/redis.conf /data/redis.conf

# The password reaches awk through the environment and is replaced with
# index/substr rather than gsub, so & and \ in the value stay literal.
if [ -n "${REDIS_PASSWORD:-}" ]; then
    awk 'BEGIN { pass = ENVIRON["REDIS_PASSWORD"]; ph = "${REDIS_PASSWORD}" }
         {
             out = ""
             rest = $0
             while ((i = index(rest, ph)) > 0) {
                 out = out substr(rest, 1, i - 1) pass
                 rest = substr(rest, i + length(ph))
             }
             print out rest
         }' /data/redis.conf > /data/redis.conf.tmp
    mv /data/redis.conf.tmp /data/redis.conf
fi

# A staged restore payload means the operator shut this master down to load a
# backup. The previous AOF is moved aside (not deleted) so it cannot shadow
# the dump, and AOF is disabled for this boot because redis-server with
# appendonly yes never loads dump.rdb. The operator re-enables AOF after it
# verified the restored dataset.
if [ -f /data/redguard-restore.rdb ]; then
    log "restore payload found; loading it instead of the previous dataset"
    rm -rf /data/appendonlydir.pre-restore
    if [ -d /data/appendonlydir ]; then
        mv /data/appendonlydir /data/appendonlydir.pre-restore
    fi
    mv /data/redguard-restore.rdb /data/dump.rdb
    printf '\nappendonly no\n' >> /data/redis.conf
fi

is_ipv4() {
    printf '%%s\n' "$1" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$'
}

# Prints only the master IP on stdout; progress goes to stderr so command
# substitution captures the bare value.
get_master_from_sentinel() {
    for sentinel in $(echo "$SENTINEL_HOSTS" | tr ',' ' '); do
        log "querying sentinel $sentinel"
        reply=$(REDISCLI_AUTH="${REDIS_PASSWORD:-}" redis-cli -h "$sentinel" -p 26379 \
            SENTINEL get-master-addr-by-name "$MASTER_NAME" 2>/dev/null | head -1 | tr -d '"\r') || reply=""
        if [ -n "$reply" ] && [ "$reply" != "nil" ] && is_ipv4 "$reply"; then
            log "sentinel $sentinel reports master $reply"
            printf '%%s' "$reply"
            return 0
        fi
    done
    return 1
}

CURRENT_MASTER=""
i=1
while [ "$i" -le 30 ]; do
    # An assignment used as an if condition does not trip set -e on failure.
    if CURRENT_MASTER=$(get_master_from_sentinel); then
        break
    fi
    CURRENT_MASTER=""
    log "sentinel not ready yet (attempt $i/30)"
    sleep 2
    i=$((i + 1))
done

grep -Ev '^(replicaof|slaveof)' /data/redis.conf > /data/redis.conf.tmp || true
mv /data/redis.conf.tmp /data/redis.conf

if [ -n "$CURRENT_MASTER" ]; then
    if [ "$CURRENT_MASTER" = "$MY_IP" ]; then
        log "sentinel confirms this pod as master"
    else
        log "configuring as replica of $CURRENT_MASTER"
        printf '\nreplicaof %%s 6379\n' "$CURRENT_MASTER" >> /data/redis.conf
    fi
elif [ "$POD_ORDINAL" = "0" ]; then
    log "no sentinel reachable; ordinal 0 bootstraps as initial master"
else
    log "no sentinel reachable; following bootstrap master $BOOTSTRAP_MASTER_HOST"
    printf '\nreplicaof %%s 6379\n' "$BOOTSTRAP_MASTER_HOST" >> /data/redis.conf
fi

log "replication config: $(grep -E '^(replicaof|slaveof)' /data/redis.conf || echo master)"
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
		"dir /data",
		// Directives apply in order: resolve-hostnames must be set before the
		// monitor line or sentinel rejects its hostname target at parse time.
		"sentinel resolve-hostnames yes",
		"sentinel announce-hostnames no",
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
			"init.sh":       buildSentinelInitScript(masterHost),
		},
	}
}

// buildSentinelInitScript creates the sentinel startup script. Invariants the
// script must hold: existing state on the PVC is kept verbatim (sentinel wrote
// the learned master, replicas and epoch into it, and its password is already
// literal), a seed is published atomically only after the master hostname
// resolved, an unresolvable master is fatal rather than a silent fall-through,
// and the state file holding the password is never readable beyond its owner.
func buildSentinelInitScript(masterHost string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu

log() { echo "[redguard-sentinel-init] $*" >&2; }

STATE=/data/sentinel.conf
SEED=/data/sentinel.conf.seed

umask 077

if [ -f "$STATE" ]; then
    log "existing sentinel state found; keeping it"
else
    log "seeding sentinel config from template"
    cp /etc/sentinel/sentinel.conf "$SEED"
    chmod 600 "$SEED"

    # The password reaches awk through the environment and is replaced with
    # index/substr rather than gsub, so & and \ in the value stay literal.
    if [ -n "${REDIS_PASSWORD:-}" ]; then
        awk 'BEGIN { pass = ENVIRON["REDIS_PASSWORD"]; ph = "${REDIS_PASSWORD}" }
             {
                 out = ""
                 rest = $0
                 while ((i = index(rest, ph)) > 0) {
                     out = out substr(rest, 1, i - 1) pass
                     rest = substr(rest, i + length(ph))
                 }
                 print out rest
             }' "$SEED" > "$SEED.tmp"
        mv "$SEED.tmp" "$SEED"
    fi

    MASTER_HOST="%s"
    log "resolving master host $MASTER_HOST"
    MASTER_IP=""
    i=1
    while [ "$i" -le 30 ]; do
        MASTER_IP=$(getent hosts "$MASTER_HOST" 2>/dev/null | awk '{ print $1 }' | head -1) || MASTER_IP=""
        if [ -n "$MASTER_IP" ]; then
            break
        fi
        log "waiting for master DNS ($i/30)"
        sleep 2
        i=$((i + 1))
    done
    if [ -z "$MASTER_IP" ]; then
        # Leave nothing behind: the next start must retry the seed instead of
        # keeping a monitor target that never resolved.
        rm -f "$SEED"
        log "FATAL: cannot resolve $MASTER_HOST; refusing to start with an unresolvable monitor target"
        exit 1
    fi
    log "master resolved to $MASTER_IP"
    awk -v host="$MASTER_HOST" -v ip="$MASTER_IP" '{ gsub(host, ip); print }' "$SEED" > "$SEED.tmp"
    mv "$SEED.tmp" "$SEED"

    # Publish atomically so a crash mid-seed cannot leave a half-built state
    # file that the next start would then keep.
    mv "$SEED" "$STATE"
fi

chmod 600 "$STATE"
exec redis-sentinel "$STATE"
`, masterHost)
}

// reservedDirectives are redis.conf/sentinel.conf directives the operator owns
// or that would subvert authentication, transport, replication topology or the
// server process itself. Redis parses directive names case-insensitively, so
// lookups happen on the lowercased name.
var reservedDirectives = map[string]struct{}{
	"requirepass":              {},
	"masterauth":               {},
	"masteruser":               {},
	"user":                     {},
	"aclfile":                  {},
	"port":                     {},
	"bind":                     {},
	"dir":                      {},
	"include":                  {},
	"loadmodule":               {},
	"rename-command":           {},
	"unixsocket":               {},
	"unixsocketperm":           {},
	"protected-mode":           {},
	"enable-protected-configs": {},
	"enable-debug-command":     {},
	"enable-module-command":    {},
	"replicaof":                {},
	"slaveof":                  {},
	"replica-announce-ip":      {},
	"replica-announce-port":    {},
	"slave-announce-ip":        {},
	"slave-announce-port":      {},
}

// reservedSentinelDirectives are the "sentinel <name>" directives the operator
// owns; overriding them re-points monitoring or leaks credentials.
var reservedSentinelDirectives = map[string]struct{}{
	"monitor":            {},
	"auth-pass":          {},
	"auth-user":          {},
	"announce-ip":        {},
	"announce-port":      {},
	"announce-hostnames": {},
	"resolve-hostnames":  {},
	"rename-command":     {},
	"sentinel-user":      {},
	"sentinel-pass":      {},
}

// configTokenPattern is the shape of one directive-name token.
var configTokenPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)

// ValidateCustomConfig rejects customConfig entries that could smuggle extra
// directives past the renderer or override directives the operator owns. A key
// is one directive name, or "sentinel <name>" for sentinel directives; values
// may not contain line breaks, which is what keeps the "<key> <value>" line
// rendering injection-proof.
func ValidateCustomConfig(cfg map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(cfg)) {
		value := cfg[key]
		if strings.ContainsAny(key, "\n\r") || strings.ContainsAny(value, "\n\r") {
			return fmt.Errorf("key %q: keys and values must not contain line breaks", key)
		}
		fields := strings.Fields(key)
		if len(fields) == 0 {
			return fmt.Errorf("key %q: empty directive name", key)
		}
		for _, f := range fields {
			if !configTokenPattern.MatchString(f) {
				return fmt.Errorf("key %q: %q is not a valid directive token", key, f)
			}
		}
		directive := strings.ToLower(fields[0])
		switch {
		case directive == "sentinel":
			if len(fields) != 2 {
				return fmt.Errorf("key %q: sentinel directives take the form \"sentinel <name>\"", key)
			}
			if _, reserved := reservedSentinelDirectives[strings.ToLower(fields[1])]; reserved {
				return fmt.Errorf("key %q is reserved: the operator manages this directive", key)
			}
		case len(fields) != 1:
			return fmt.Errorf("key %q: a key must be a single directive name", key)
		case strings.HasPrefix(directive, "tls-"):
			return fmt.Errorf("key %q is reserved: TLS is configured via spec.tls", key)
		default:
			if _, reserved := reservedDirectives[directive]; reserved {
				return fmt.Errorf("key %q is reserved: the operator manages this directive", key)
			}
		}
	}
	return nil
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
