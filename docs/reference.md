# Custom resource reference

API group and version: `redis.redguard.io/v1alpha1`.

All four kinds are namespaced. Every reference between them — a cluster name, a
Secret name — resolves in the referring object's own namespace. There is no
cross-namespace reference.

- [RedisSentinel](#redissentinel)
- [RedisUser](#redisuser)
- [RedisBackup](#redisbackup)
- [RedisRestore](#redisrestore)

---

## RedisSentinel

One Redis master-replica set and the Sentinels that supervise it.

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: redis-cluster
spec:
  redisConfig:
    replicas: 3
    image: redis:7-alpine
    auth:
      secretName: redis-password
    storage:
      size: 10Gi
      storageClassName: gp3
    resources:
      requests: {cpu: 200m, memory: 512Mi}
      limits: {cpu: 1, memory: 2Gi}
    customConfig:
      maxmemory: "1gb"
      maxmemory-policy: allkeys-lru
  sentinelConfig:
    replicas: 3
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 10000
    parallelSyncs: 1
  serviceType: ClusterIP
```

### spec.redisConfig

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `replicas` | integer | `3` | Redis instances, master included. Minimum 1. One is legal but leaves Sentinel nothing to promote. |
| `image` | string | `redis:7-alpine` | Redis image. |
| `resources` | ResourceRequirements | none | Applied to the Redis container. |
| `storage.size` | Quantity | `1Gi` | Size of each pod's PVC. |
| `storage.storageClassName` | string | cluster default | StorageClass for the Redis and Sentinel PVCs. |
| `customConfig` | map[string]string | none | Extra `redis.conf` directives. |
| `auth.secretName` | string | none | Secret in the same namespace with the admin password under `password`. |
| `nodeSelector` | map[string]string | none | Node labels the Redis pods require. |
| `affinity` | Affinity | preferred anti-affinity | Replaces the scheduling rules. See [placement](#placement). |
| `tolerations` | []Toleration | none | Taints the Redis pods tolerate. |
| `topologySpreadConstraints` | []TopologySpreadConstraint | none | Spread across zones or other domains. |
| `priorityClassName` | string | none | Scheduling priority and eviction order. |

`spec.redisConfig.storage` is **immutable**, and so is its presence. A
StatefulSet volume claim template cannot be resized or moved to another class
after creation, so both are rejected at admission:

```
spec.redisConfig.storage: Invalid value: redisConfig.storage is immutable: a
StatefulSet volumeClaimTemplate cannot be resized or moved to another storage
class after creation.
```

To grow a volume, resize the PersistentVolumeClaims directly if the
StorageClass allows expansion; otherwise recreate the `RedisSentinel`.

`customConfig` keys are single directive names. Keys and values may not contain
line breaks, and the directives the operator owns are rejected at reconcile with
`Degraded`, reason `InvalidSpec`:

`requirepass`, `masterauth`, `masteruser`, `user`, `aclfile`, `port`, `bind`,
`dir`, `include`, `loadmodule`, `rename-command`, `unixsocket`,
`unixsocketperm`, `protected-mode`, `enable-protected-configs`,
`enable-debug-command`, `enable-module-command`, `replicaof`, `slaveof`,
`replica-announce-ip`, `replica-announce-port`, `slave-announce-ip`,
`slave-announce-port`, and anything starting with `tls-`.

### spec.sentinelConfig

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `replicas` | integer | `3` | Sentinel instances. Minimum 3. |
| `quorum` | integer | `2` | Sentinels that must agree the master is down. Minimum 2, and never more than `replicas`. |
| `downAfterMilliseconds` | integer | `5000` | Time before an instance is considered down. Minimum 1. |
| `failoverTimeout` | integer | `10000` | Failover timeout in milliseconds. Minimum 1. |
| `parallelSyncs` | integer | `1` | Replicas resynchronised in parallel after a promotion. Minimum 1. |
| `resources` | ResourceRequirements | none | Applied to the Sentinel container. |
| `customConfig` | map[string]string | none | Extra `sentinel.conf` directives. Keys are a directive name or `sentinel <name>`. |
| `nodeSelector`, `affinity`, `tolerations`, `topologySpreadConstraints`, `priorityClassName` | | | Same as `redisConfig`, applied to the Sentinel pods only. |

`quorum > replicas` is rejected at admission: a quorum larger than the Sentinel
set can never be reached, so no failover could ever start.

The reserved `sentinel <name>` directives are `monitor`, `auth-pass`,
`auth-user`, `announce-ip`, `announce-port`, `announce-hostnames`,
`resolve-hostnames`, `rename-command`, `sentinel-user` and `sentinel-pass`.

Sentinel keeps its learned state — master, replicas, epochs — on a PVC and reads
the ConfigMap template only on first boot. Editing `quorum` or the timers is
therefore applied at runtime with `SENTINEL SET` on every member, not by
restarting the pods. The result is reported as the `SentinelConfigInSync`
condition.

### spec.tls

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | Serve TLS. Both servers then listen on TLS only. |
| `certificateSecretRef` | string | none | Secret with `tls.crt` and `tls.key`. Required when `enabled`. |
| `caSecretRef` | string | none | Secret with `ca.crt`. Unset verifies against the system trust store. |
| `mutualTLS` | bool | `false` | Sets `tls-auth-clients yes`: clients must present a certificate. |

There is no `clientCertRequired` field; `mutualTLS` is the field that requires a
client certificate.

TLS clusters do not currently reach `Running`. See
[operations.md](operations.md#tls).

### spec.serviceType

`ClusterIP` (default), `NodePort` or `LoadBalancer`. It applies to
`<name>-redis` and `<name>-redis-replicas`. The Sentinel Service is always
`ClusterIP`.

### Placement

Both `redisConfig` and `sentinelConfig` carry the same placement fields, applied
to that component only. The two are scheduled independently.

When `affinity.podAntiAffinity` is absent, the operator adds a preferred
anti-affinity that spreads the component's pods across nodes. It degrades to
co-location on a cluster with fewer nodes than replicas rather than leaving pods
Pending. Setting `podAntiAffinity` replaces it; set it to `{}` to opt out.
Setting only `nodeAffinity` leaves the default spreading in place.

```yaml
spec:
  redisConfig:
    affinity:
      podAntiAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              app.kubernetes.io/instance: redis-cluster
              app.kubernetes.io/component: redis
          topologyKey: kubernetes.io/hostname
  sentinelConfig:
    topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          app.kubernetes.io/instance: redis-cluster
          app.kubernetes.io/component: sentinel
```

### status

| Field | Notes |
| --- | --- |
| `phase` | `Creating`, `Scaling`, `ConfiguringSentinel`, `Running` or `Degraded`. |
| `observedGeneration` | The spec generation this status was computed from. |
| `masterNode` | `host:port` of the current master as Sentinel reports it. The host is the pod IP. |
| `readyReplicas` | Ready Redis pods. |
| `readySentinels` | Ready Sentinel pods. |
| `lastFailoverTime` | When the master last changed. |
| `conditions` | Below. |

| Condition | True means |
| --- | --- |
| `Available` | `phase` is `Running`. |
| `Degraded` | The spec was rejected at reconcile. Only a spec edit clears it. |
| `HighlyAvailable` | `redisConfig.replicas` is at least 2, so Sentinel has a replica to promote. |
| `ReplicationHealthy` | Every Redis replica is ready. |
| `SentinelHealthy` | Sentinel itself reports a healthy quorum. |
| `SentinelConfigInSync` | Every running Sentinel applies the monitor parameters in the spec. |

Print columns: `PHASE`, `MASTER`, `REPLICAS`, `SENTINELS`, `AGE`.

---

## RedisUser

One Redis ACL user, applied to every ready Redis node so it survives a failover
and a pod restart.

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisUser
metadata:
  name: app-user
spec:
  redisClusterRef: redis-cluster
  username: app-user
  passwordSecretRef: app-user-password
  enabled: true
  aclRules:
    categories: ["+@read", "+@write"]
    commands: ["+get", "+set", "+del"]
    keys: ["~app:*", "~cache:*"]
    channels: ["&notifications:*"]
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `redisClusterRef` | string | yes | `RedisSentinel` in the same namespace. |
| `username` | string | yes | 1-64 characters, `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`. `default` is reserved and rejected at admission. |
| `passwordSecretRef` | string | yes | Secret in the same namespace with the password under `password`. |
| `enabled` | bool | no, default `true` | `false` renders the account `off`: it keeps its rules but every AUTH is rejected. |
| `aclRules.categories` | []string | no | `+@read`, `+@write`, ... |
| `aclRules.commands` | []string | no | `+get`, `-flushdb`, ... |
| `aclRules.keys` | []string | no | `~app:*`, `%R~cache:*`, ... |
| `aclRules.channels` | []string | no | `&notifications:*` |

Nothing is granted implicitly. A user with no rules can authenticate and reach
no key, channel or command.

Rules are single tokens; whitespace and Redis 7 selector syntax `(...)` are
rejected at admission. Password rules (`>`, `<`, `#`, `!`) are rejected: the
password comes from `passwordSecretRef`. What else is allowed, and the
confinement floor the operator appends, is in
[security.md](security.md#what-the-acl-allowlist-confines).

### status

| Field | Notes |
| --- | --- |
| `phase` | `Ready`, `Error` or `Degraded`. |
| `observedGeneration` | The spec generation this status was computed from. |
| `appliedTo` | Addresses of the nodes carrying the user. |
| `lastPasswordChange` | When the password was last written. |
| `conditions` | `Ready`, and `Degraded` for a rejected spec. |

Print columns: `USERNAME`, `CLUSTER`, `ENABLED`, `PHASE`, `AGE`.

---

## RedisBackup

An RDB snapshot of a cluster, uploaded to S3 or an S3-compatible endpoint. With
a `schedule` it repeats; without one it runs once.

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: daily
spec:
  redisClusterRef: redis-cluster
  schedule: "0 2 * * *"
  s3:
    bucket: acme-redis-backups
    region: eu-west-1
    prefix: production
    credentialsSecretRef: s3-credentials
  retentionPolicy: 7
  compression: true
  suspend: false
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `redisClusterRef` | string | yes | `RedisSentinel` in the same namespace. Part of the object key. |
| `schedule` | string | no | Five-field cron, optionally prefixed `TZ=` or `CRON_TZ=`. Descriptors such as `@daily` are not accepted. Empty runs once. |
| `s3.bucket` | string | yes | Destination bucket. |
| `s3.region` | string | no | AWS region. |
| `s3.endpoint` | string | no | S3-compatible endpoint, for example MinIO. |
| `s3.prefix` | string | no | Prefix under which this backup writes and prunes. |
| `s3.credentialsSecretRef` | string | one of | Secret with `accessKeyId` and `secretAccessKey`. |
| `s3.useIAMRole` | bool | one of | Sign with the operator's own AWS identity. Requires an allowlisted destination. |
| `retentionPolicy` | integer | no, default `7` | Backups of this cluster to keep. `0` keeps every backup. |
| `compression` | bool | no, default `true` | gzip the RDB. |
| `suspend` | bool | no | Pause the schedule. |

Exactly one of `s3.credentialsSecretRef` and `s3.useIAMRole` must be set. With
neither, the AWS SDK would sign with whatever ambient identity the operator pod
carries and never pass the destination allowlist; with both, the Secret wins and
`useIAMRole` says nothing about which identity signed. Admission rejects both
cases.

Objects are written to:

```
<prefix>/<namespace>/<cluster>/backup-<YYYYMMDD-HHMMSS>.rdb[.gz]
```

The namespace and cluster segments are always present, so two clusters can never
share a prefix. Retention only ever deletes objects directly under that exact
prefix whose name matches `backup-*.rdb` or `backup-*.rdb.gz`. Anything else in
the bucket is left alone.

The cron `schedule` pattern only fixes the shape. An out-of-range field is
caught by the controller and reported as `Degraded`.

### status

| Field | Notes |
| --- | --- |
| `phase` | `Running`, `Completed`, `Failed`, `Suspended` or `Degraded`. |
| `observedGeneration` | The spec generation this status was computed from. |
| `lastBackupTime`, `nextBackupTime` | Last successful run and next scheduled run. |
| `backupLocation` | `s3://bucket/key` of the last backup. |
| `backupSize` | Bytes uploaded. |
| `keyCount` | Keys across all databases when the snapshot was taken. `RedisRestore` verifies against it. |
| `backupCount` | Backups retained under the prefix. |
| `lastBackupDuration` | Duration of the last run. |
| `conditions` | `Ready`, and `Degraded` for a rejected spec. A refused destination sets `Ready=False` with reason `DestinationNotAllowed`. |

Print columns: `CLUSTER`, `SCHEDULE`, `PHASE`, `LAST BACKUP`, `AGE`.

A failed run is retried on a 30 minute interval, not immediately: a permanently
failing backup would otherwise re-fire `BGSAVE` against the master in a tight
loop.

---

## RedisRestore

Loads one S3 object back into a cluster's master. Replicas resynchronise from
the master afterwards.

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisRestore
metadata:
  name: restore-2026-08-08
spec:
  redisClusterRef: redis-cluster
  backupSource:
    s3:
      bucket: acme-redis-backups
      region: eu-west-1
      credentialsSecretRef: s3-credentials
    backupPath: production/default/redis-cluster/backup-20260808-020000.rdb.gz
    compressed: true
  force: false
  skipDataCheck: false
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `redisClusterRef` | string | yes | `RedisSentinel` in the same namespace. |
| `backupSource.s3` | S3Config | yes | Same shape and same one-of rule as `RedisBackup`. |
| `backupSource.backupPath` | string | yes | Object key inside the bucket, not a `s3://` URL. |
| `backupSource.compressed` | bool | no, default `true` | The object is gzip compressed. |
| `force` | bool | no | Restore even though the cluster already holds data. Overwrites it. |
| `skipDataCheck` | bool | no | Skip the pre-restore data check entirely. |

Without `force`, a restore into a cluster that already holds keys is refused
before anything is touched.

The restore runs in phases, each safe to re-enter after a requeue or an operator
restart: `Pending` → `Downloading` → `Restoring` → `Verifying` → `Completed`.
The payload is downloaded, checked for the `REDIS` magic, and staged on the
master's volume under a name the running server never reads. The Sentinels are
told to hold off failure detection, the master is shut down with `SHUTDOWN
NOSAVE`, and its start-up script swaps the payload in. Durability and failure
detection are restored only after the reloaded dataset is verified against the
key count the `RedisBackup` recorded.

A `Completed` or `Failed` restore does not re-run until the spec changes.

### status

| Field | Notes |
| --- | --- |
| `phase` | `Pending`, `Downloading`, `Restoring`, `Verifying`, `Completed`, `Failed`. |
| `observedGeneration` | The spec generation the recorded phase applies to. |
| `quiescedDownAfterMilliseconds` | The Sentinel value to put back once the restart is over. Zero means the Sentinels are not quiesced. |
| `startTime`, `completionTime`, `duration` | Timing. |
| `restoredFrom` | The object key that was loaded. |
| `restoredDataSize` | Bytes of RDB loaded. |
| `message` | Human-readable state, including the reason for a failure. |
| `conditions` | `Ready`. |

Print columns: `CLUSTER`, `PHASE`, `DURATION`, `AGE`.
