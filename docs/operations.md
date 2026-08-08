# Operations

Day-two work on a running cluster. Every command below assumes a `RedisSentinel`
named `redis-cluster` in the current namespace.

- [Connecting](#connecting)
- [Watching a cluster](#watching-a-cluster)
- [Failover](#failover)
- [Scaling](#scaling)
- [Changing configuration](#changing-configuration)
- [Rotating the password](#rotating-the-password)
- [ACL users](#acl-users)
- [TLS](#tls)
- [Backup](#backup)
- [Restore](#restore)
- [Metrics](#metrics)
- [Alerts](#alerts)
- [Troubleshooting](#troubleshooting)

## Connecting

Two client Services, with different guarantees.

`redis-cluster-redis` selects only the pod carrying
`redis.redguard.io/role: master`. Use it for everything that writes. Between a
master failing and the operator moving the label, the Service has no endpoints
and connections fail fast rather than reaching a read-only replica.

`redis-cluster-redis-replicas` selects every Redis pod, master included. Use it
for read-only traffic. A write sent here can land on a replica and come back
`-READONLY`.

```sh
kubectl run redis-client --rm -i --restart=Never --image redis:7-alpine -- \
  redis-cli -h redis-cluster-redis SET greeting hello

kubectl run redis-client --rm -i --restart=Never --image redis:7-alpine -- \
  redis-cli -h redis-cluster-redis-replicas GET greeting
```

A Sentinel-aware client discovers the master itself. The Sentinel Service is
`redis-cluster-sentinel:26379` and the monitor name is `redis-cluster-master`:

```sh
kubectl run redis-client --rm -i --restart=Never --image redis:7-alpine -- \
  redis-cli -h redis-cluster-sentinel -p 26379 \
  SENTINEL get-master-addr-by-name redis-cluster-master
```

With authentication enabled, both ports require the password, Sentinel included.

## Watching a cluster

```sh
kubectl get redissentinel redis-cluster
kubectl get redissentinel redis-cluster -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
kubectl get pods -l app.kubernetes.io/instance=redis-cluster
kubectl get pods -l app.kubernetes.io/instance=redis-cluster,redis.redguard.io/role=master
kubectl get endpoints redis-cluster-redis
kubectl logs -n redguard-system -l control-plane=controller-manager -f
```

The operator records events on the `RedisSentinel`:

```sh
kubectl describe redissentinel redis-cluster | sed -n '/Events/,$p'
```

Reasons worth knowing: `FailoverDetected`, `FailoverHandled`, `ConfigRollout`,
`CredentialsRotated`, `CredentialRotationFailed`, `ScaleDownDeferred`,
`SentinelScaleDown`, `SentinelConfigRepaired`, `SentinelPeersReset`,
`InvalidCustomConfig`.

## Failover

Sentinel promotes a replica on its own. The operator notices on its next pass,
records `FailoverDetected`, reconfigures the surviving replicas to follow the
new master, moves the `redis.redguard.io/role: master` label — which repoints
`redis-cluster-redis` — and sets `status.lastFailoverTime`.

Force one by deleting the master pod:

```sh
kubectl delete pod "$(kubectl get pods -l app.kubernetes.io/instance=redis-cluster,redis.redguard.io/role=master -o name | head -1 | cut -d/ -f2)"
kubectl get redissentinel redis-cluster -w
```

Expect the promotion within `downAfterMilliseconds` plus the election, and the
label to follow within a few seconds after that. On a default 3+3 cluster the
whole sequence takes about 35 seconds.

While the operator is down the label cannot move, so `redis-cluster-redis` keeps
selecting the demoted pod until the operator comes back. Sentinel-aware clients
are unaffected: they ask Sentinel.

## Scaling

```sh
kubectl patch redissentinel redis-cluster --type merge \
  -p '{"spec":{"redisConfig":{"replicas":5}}}'
```

Scaling down removes the highest ordinals. If the master sits on one of them,
the operator holds the scale-down, asks Sentinel to promote a replica that
survives, and records `ScaleDownDeferred`. Deleting the master outright would
also work, but only after `downAfterMilliseconds` plus an election, during which
writes fail and any acknowledged write that had not reached a replica is gone.

Scaling Sentinel down records `SentinelScaleDown`. Removed Sentinels stay in the
survivors' view until the operator issues `SENTINEL RESET`, which it does once
the peer count is provably stale. Keep the Sentinel count odd and at least 3, and
keep `quorum` at `replicas / 2 + 1`.

PersistentVolumeClaims of removed pods are not deleted. Remove them by hand if
you do not intend to scale back up.

## Changing configuration

```sh
kubectl patch redissentinel redis-cluster --type merge -p '{
  "spec": {"redisConfig": {"customConfig": {
    "maxmemory": "2gb",
    "maxmemory-policy": "allkeys-lru"
  }}}}'
```

The operator hashes the generated `redis.conf` into a pod annotation, so the
StatefulSet rolls the pods. It records `ConfigRollout` before the roll starts.
A rolling restart of a Redis StatefulSet moves the master at least once.

Sentinel parameters are different. Sentinel rewrites its own config file with
what it has learned and reads the template only on first boot, so editing
`quorum` or the timers cannot be applied by rolling the pods. The operator reads
the live values from every Sentinel and repairs them with `SENTINEL SET`,
records `SentinelConfigRepaired`, and reports the outcome as
`SentinelConfigInSync`:

```sh
kubectl patch redissentinel redis-cluster --type merge \
  -p '{"spec":{"sentinelConfig":{"downAfterMilliseconds":3000}}}'
kubectl get redissentinel redis-cluster \
  -o jsonpath='{.status.conditions[?(@.type=="SentinelConfigInSync")].message}'
```

`spec.redisConfig.storage` cannot be changed at all. See
[reference.md](reference.md#specredisconfig).

## Rotating the password

Update the Secret. Nothing else.

```sh
kubectl create secret generic redis-password \
  --from-literal=password="$(openssl rand -base64 24)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

The operator pushes the new password to every running node online, in three
cluster-wide phases: every node accepts both passwords, every node presents the
new one, the old one stops being accepted. Replication links and Sentinel
monitoring stay up throughout. Only then does it stamp the new Secret version on
the pod templates, so the roll that follows makes the change durable rather than
performing it.

It records `CredentialsRotated` on success. On a partial push it records
`CredentialRotationFailed` and rolls nothing: the cluster still agrees on the
previously applied password and the push is retried.

The password the cluster currently accepts is kept in an operator-owned Secret
named `<cluster>-auth-state`, because during a rotation the user's Secret already
holds the new value while the nodes still require the old one. It is owned by the
`RedisSentinel` and deleted with it. Do not edit it.

## ACL users

```sh
kubectl create secret generic app-user-password --from-literal=password='...'
kubectl apply -f - <<'EOF'
apiVersion: redis.redguard.io/v1alpha1
kind: RedisUser
metadata:
  name: app-user
spec:
  redisClusterRef: redis-cluster
  username: app-user
  passwordSecretRef: app-user-password
  aclRules:
    categories: ["+@read", "+@write"]
    keys: ["~app:*"]
EOF
kubectl get redisuser app-user
```

The user is written to every ready Redis node and saved to that node's ACL file,
so it survives a failover and a pod restart. `status.appliedTo` lists the nodes.

Disable without deleting:

```sh
kubectl patch redisuser app-user --type merge -p '{"spec":{"enabled":false}}'
```

Rules that would escape the user's declared scope are rejected, and the operator
appends a confinement floor to every grant. See
[security.md](security.md#what-the-acl-allowlist-confines) for what is allowed
and why.

## TLS

`spec.tls` is accepted by the API and the generated configuration is complete:
Redis and Sentinel are switched to `tls-port`, the certificate is mounted, and
the probes speak TLS. The operator also stamps the certificate Secret's version
on both pod templates, so renewing the certificate rolls the pods — servers load
their certificates once, at start-up, and a renewal reaches a running pod no
other way.

`caSecretRef` may name the certificate Secret or a separate one; both shapes are
mounted into a single directory, so the paths in the configuration, the probes
and the start-up script do not change.

The operator neither issues nor renews certificates. Supply them yourself or
through cert-manager; a renewal rolls the pods.

## Backup

```sh
kubectl create secret generic s3-credentials \
  --from-literal=accessKeyId=AKIA... \
  --from-literal=secretAccessKey=...

kubectl apply -f - <<'EOF'
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
EOF

kubectl get redisbackup daily
```

```
NAME    CLUSTER         SCHEDULE    PHASE       LAST BACKUP   AGE
daily   redis-cluster   0 2 * * *   Completed   40s           2m
```

Omit `schedule` for a one-off. The object lands at
`<prefix>/<namespace>/<cluster>/<backupName>/backup-<timestamp>.rdb.gz`, and
`status.backupLocation` gives the exact `s3://` URL to feed a restore.

Pause a schedule with `spec.suspend: true`. Retention deletes only objects this
controller wrote under that exact prefix.

On EKS, set `spec.s3.useIAMRole: true` instead of a credentials Secret and
annotate the operator's ServiceAccount with the IRSA role. The bucket must then
be in the operator's `--allowed-backup-buckets`; otherwise the backup is refused
with `Ready=False`, reason `DestinationNotAllowed`. See
[install.md](install.md#allowing-iam-role-backups).

## Restore

```sh
kubectl apply -f - <<'EOF'
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
  force: true
EOF

kubectl get redisrestore restore-2026-08-08 -w
```

```
NAME                 CLUSTER         PHASE       DURATION        AGE
restore-2026-08-08   redis-cluster   Completed   17.329594751s   38s
```

`backupPath` is the object key inside the bucket, not an `s3://` URL. Without
`force`, a cluster that already holds keys is refused before anything is
touched.

The restore restarts the master. The Sentinels are told to hold off failure
detection first, but that setting is converged back towards the spec by the
`RedisSentinel` controller within one reconcile pass, so the protection is not
reliable: if the master's restart outlasts `sentinelConfig.downAfterMilliseconds`
plus an election, Sentinel promotes a replica, the restarted pod rejoins as a
replica of it, and the restored dataset is discarded by the resynchronisation.
The restore then fails verification:

```
Restored key count 0 does not match the 2 recorded by the backup
```

Always check that `status.phase` reached `Completed`. A `Failed` restore has not
loaded anything durable; re-run it by editing the spec.

## Metrics

The operator serves Prometheus metrics on the `<release>-metrics` Service —
`redguard-metrics` for the default release name — on port 8080. Installed from
the release manifest instead of the chart, it is
`redguard-controller-manager-metrics-service` on 8443.

The endpoint is **HTTPS with token authentication and a SubjectAccessReview
check**, not plain HTTP. A scraper needs a token whose subject may `get` the
`/metrics` non-resource URL.

```sh
kubectl create serviceaccount metrics-reader -n redguard-system
kubectl create clusterrole metrics-reader --non-resource-url=/metrics --verb=get
kubectl create clusterrolebinding metrics-reader \
  --clusterrole=metrics-reader \
  --serviceaccount=redguard-system:metrics-reader

TOKEN=$(kubectl create token metrics-reader -n redguard-system)
kubectl port-forward -n redguard-system svc/redguard-metrics 8443:8080 &
curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:8443/metrics | grep '^redis_'
```

`-k` is needed because controller-runtime generates a self-signed certificate
when none is configured.

Cluster series, labelled `namespace` and `name`:

| Metric | Type | Meaning |
| --- | --- | --- |
| `redis_cluster_info` | gauge | 1 up, 0 down |
| `redis_connected_replicas` | gauge | Replicas connected to the master |
| `redis_failover_total` | counter | Failovers observed |
| `redis_sentinel_status` | gauge | 1 healthy, 0 unhealthy |
| `redis_sentinel_quorum_healthy` | gauge | 1 quorum reachable, 0 not |
| `redis_sentinel_monitored_masters` | gauge | Masters Sentinel monitors |
| `redis_sentinel_known_sentinels` | gauge | Other Sentinels known for the master |
| `redis_sentinel_known_replicas` | gauge | Replicas known for the master |

Per-pod series, additionally labelled `pod`:

| Metric | Type | Meaning |
| --- | --- | --- |
| `redis_replication_lag_seconds` | gauge | Lag in seconds; `-1` when the replica could not be queried |
| `redis_replication_lag_bytes` | gauge | `master_repl_offset - slave_repl_offset` |
| `redis_replication_offset` | gauge | Replication offset, also labelled `role` |
| `redis_master_link_status` | gauge | 1 up, 0 down |
| `redis_used_memory_bytes`, `redis_max_memory_bytes` | gauge | `max` is 0 when unlimited |
| `redis_memory_fragmentation_ratio` | gauge | `used_memory_rss / used_memory` |
| `redis_connected_clients`, `redis_blocked_clients` | gauge | Client counts |
| `redis_rejected_connections_total` | counter | Connections refused by `maxclients` |
| `redis_rdb_last_save_timestamp_seconds` | gauge | Last successful RDB save |
| `redis_rdb_changes_since_last_save` | gauge | Unsaved changes |
| `redis_aof_enabled`, `redis_aof_current_size_bytes` | gauge | AOF state |
| `redis_total_commands_processed` | counter | Commands processed |
| `redis_keyspace_hits_total`, `redis_keyspace_misses_total` | counter | Lookups |
| `redis_db_keys` | gauge | Keys per database, labelled `db` |

Backup and ACL series:

| Metric | Type | Meaning |
| --- | --- | --- |
| `redis_backup_status` | gauge | `1` last run succeeded, `0` failed, `-1` a run is in flight. Labelled `namespace`, `name`, `cluster`, where `name` is the `RedisBackup` |
| `redis_backup_duration_seconds` | histogram | Backup duration |
| `redis_backup_size_bytes` | gauge | Bytes uploaded |
| `redis_user_acl_status` | gauge | 1 applied, 0 error. Labelled `namespace`, `name`, `username` |

Counters sourced from `INFO` are advanced by their delta, so a Redis restart
does not look like a burst to `rate()` and `increase()`.

Series are deleted when their object is: a removed `RedisSentinel` drops every
cluster and per-pod series, a scale-down drops the series of the pods that are
gone, and a removed `RedisBackup` or `RedisUser` drops its own. A deleted
cluster does not keep reporting itself up.

Reconcile duration and reconcile errors are not exported by the operator.
controller-runtime already exports `controller_runtime_reconcile_time_seconds`
and `controller_runtime_reconcile_errors_total`, labelled by controller, for
every controller the manager runs.

## Alerts

`config/prometheus/prometheusrule.yaml` holds a `PrometheusRule` covering
cluster availability, quorum, replication lag, memory, persistence, backups and
failover frequency. It needs the Prometheus Operator.

```sh
kubectl apply -f config/prometheus/prometheusrule.yaml
```

The operator alerts are written against controller-runtime's counters, for
example:

```promql
increase(controller_runtime_reconcile_errors_total{controller=~"redissentinel|redisbackup|redisrestore|redisuser"}[5m]) > 5
```

## Troubleshooting

**Redis pods stay `0/1` and the log repeats `sentinel not ready yet`.** The
start-up script cannot reach Sentinel. Check the Sentinel pods are running, and
that a NetworkPolicy or a CNI is not blocking port 26379 inside the namespace. If
`spec.tls.enabled` is set, see [TLS](#tls).

**`phase: Creating` with all six pods Running.** The operator cannot reach the
pods on 6379. Check the operator log for `i/o timeout`; the usual causes are a
NetworkPolicy that does not admit the operator namespace, or the operator running
outside the cluster with `make run`, from where pod IPs are not routable.

**`phase: Degraded`.** A spec the operator rejected. The reason is in the
condition:

```sh
kubectl get redissentinel redis-cluster \
  -o jsonpath='{.status.conditions[?(@.type=="Degraded")].message}'
```

**`redis-cluster-redis` has no endpoints.** No pod carries the master role
label. Either a failover is in flight, or the operator cannot reach Sentinel to
learn who the master is. Check `status.masterNode` and the operator log.

**A `RedisUser` sits in `Error`.** Read the condition message. Common causes: the
password Secret is missing or has no `password` key, no Redis pod is running, or
an ACL rule was refused.

**A backup fails with `DestinationNotAllowed`.** The backup uses
`spec.s3.useIAMRole` and the bucket or endpoint is not in the operator's
allowlist. See [install.md](install.md#allowing-iam-role-backups).

**Operator RBAC.** A `Forbidden` in the operator log means the ClusterRole is out
of date. It is generated from the controllers' kubebuilder markers; reinstall the
chart at the version matching the image.
