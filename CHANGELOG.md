# Changelog

All notable changes to Redguard are documented in this file.

## [0.3.0] - 2026-08-08

The first release that starts. 0.2.1 and earlier could not: the chart never
granted the operator the RBAC its controllers need, so the manager failed on its
first reconcile. Everything below is written against a cluster that was booted,
failed over, backed up and restored on kind.

### Breaking changes

A 0.2.1 user will hit all of these.

- **`<name>-redis` now selects only the master.** It used to select every Redis
  pod, so writes could land on a replica and be rejected with `-READONLY`. The
  operator stamps `redis.redguard.io/role: master` on the master pod and the
  Service selects on it. During a failover the Service has no endpoints and
  writes fail fast. Read-only traffic moves to the new `<name>-redis-replicas`
  Service, which selects every Redis pod.
- **Sentinel now requires authentication.** With
  `spec.redisConfig.auth.secretName` set, `sentinel.conf` gets `requirepass` and
  `sentinel sentinel-pass` as well as `sentinel auth-pass`. Previously the
  Sentinel port was open to anything in the namespace, which could issue
  `SENTINEL FAILOVER`, `SET` or `REMOVE`. Clients that connect to port 26379 must
  now authenticate.
- **Sentinel keeps a PersistentVolumeClaim.** Each Sentinel pod gets a 1Gi
  `sentinel-data` volume, using the same StorageClass as Redis. Without durable
  state a restarted Sentinel re-seeded from the ConfigMap and re-monitored the
  bootstrap master. Each cluster now claims `sentinelConfig.replicas` more
  volumes than before.
- **`spec.redisConfig.storage` is immutable, including its presence.** A
  StatefulSet volume claim template cannot be resized or moved to another class
  after creation, and adding or removing the block changes the template.
  Admission rejects both. Resize the PersistentVolumeClaims directly or recreate
  the `RedisSentinel`.
- **`default` is a reserved username.** A `RedisUser` with
  `spec.username: default` is rejected at admission and at reconcile. It is the
  admin account whose password is `requirepass`; redefining it reset the cluster
  password and locked the operator out.
- **ACL rules are allowlisted.** `+@all`, `+@admin`, `+@dangerous`, `nopass`,
  `reset`, `on`, `off`, selector syntax and any command without a key
  specification are refused. Every category or command grant is narrowed by a
  confinement floor (`-@admin -@dangerous -scan -randomkey -dbsize -pubsub
  -function -script`). Specs that granted `+@all` or `+keys` no longer apply.
- **IAM-role backups need an allowlisted destination.** A `RedisBackup` with
  `spec.s3.useIAMRole` runs under the operator's own AWS identity, so its bucket
  must be in `--allowed-backup-buckets` and any custom endpoint in
  `--allowed-backup-endpoints`. Empty allowlists disable the IAM-role path
  entirely. Backups with `spec.s3.credentialsSecretRef` are unaffected. A spec
  with neither or both credential sources is now rejected at admission.
- **The chart grants the RBAC the operator actually needs.** The ClusterRole is
  generated from the controllers' kubebuilder markers and synced into the chart
  by `hack/sync-chart.sh`; `make verify-chart` gates the drift.
- **Backup object keys always contain the namespace and cluster.** Objects are
  written to `<prefix>/<namespace>/<cluster>/backup-<timestamp>.rdb[.gz]`, and
  retention only ever deletes objects directly under that prefix named
  `backup-*.rdb` or `backup-*.rdb.gz`. Restores of objects written by an earlier
  version need the old key in `backupPath`.
- **Two operator metrics were removed.** Reconcile duration and reconcile error
  counts are dropped in favour of controller-runtime's own
  `controller_runtime_reconcile_time_seconds` and
  `controller_runtime_reconcile_errors_total`, which are labelled by controller.
  Dashboards and alerts against the old names must be repointed.
- **`redis_backup_status` gained a third value.** `-1` now means a run is in
  flight; `1` and `0` still mean the last run succeeded or failed. An alert
  written as `redis_backup_status != 1` will fire during every backup.
- **Leader election is on by default**, and the chart refuses
  `operator.replicas > 1` without it.
- **The metrics Service is `<release>-metrics`.** It serves HTTPS with token
  authentication and a SubjectAccessReview check, not plain HTTP.

### Added

- `RedisRestore`: download an S3 backup, stage it on the master's volume, reload
  it through a controlled restart, and verify the result against the key count
  the `RedisBackup` recorded. Previously restore was a manual `kubectl cp`
  procedure in the documentation.
- `<name>-redis-replicas` Service for read-only traffic.
- PodDisruptionBudgets per component, `maxUnavailable: 1`, with
  `UnhealthyPodEvictionPolicy: AlwaysAllow` so one wedged pod cannot block a
  node drain forever.
- Placement fields on both `redisConfig` and `sentinelConfig`: `nodeSelector`,
  `affinity`, `tolerations`, `topologySpreadConstraints`, `priorityClassName`.
  A preferred pod anti-affinity spreads each component across nodes unless
  `podAntiAffinity` is set explicitly.
- Online credential rotation: a changed password is pushed to every running node
  in three cluster-wide phases before the pod templates are stamped, so
  replication and Sentinel monitoring stay up. Reported as `CredentialsRotated`
  or `CredentialRotationFailed`.
- Runtime convergence of Sentinel monitor parameters. Sentinel reads its
  template only on first boot, so `quorum` and the timers are repaired with
  `SENTINEL SET` on every member and reported as `SentinelConfigInSync`.
- Certificate rotation support: the TLS Secret version is stamped on both pod
  templates, so a renewal rolls the pods that loaded the old certificate.
- Master-safe scale-down. When the master occupies an ordinal the scale-down
  would delete, the operator holds the change and forces a promotion first,
  recording `ScaleDownDeferred`.
- `SENTINEL RESET` of removed peers on a Sentinel scale-down, so the survivors'
  majority is counted from the current set.
- `--watch-namespace` and `operator.watchNamespaces`, with label-scoped informer
  caches for the object types the operator creates.
- NetworkPolicies for both components, admitting the workload namespace and the
  operator's namespace.
- Restricted Pod Security Standard compliance for the operator and for the pods
  it creates.
- Per-pod Redis metrics (memory, clients, persistence, replication offsets,
  keyspace) and Sentinel metrics (quorum health, known sentinels and replicas).
- A `PrometheusRule` in `config/prometheus/prometheusrule.yaml`.
- Structured production logging (JSON at info level) with `--zap-devel` as the
  opt-in to console debug output, and the build version in the start-up log.
- `LICENSE`, `CONTRIBUTING.md`, `SECURITY.md`, issue and pull request templates.
- Release automation: multi-arch image, packaged chart, and `install.yaml`
  attached to the GitHub release.

### Fixed

- The chart's ClusterRole, which was the reason 0.2.1 could not reconcile
  anything.
- The Redis start-up script: it logs to stderr, survives an unreachable
  Sentinel, and no longer re-monitors the original pod after a restart.
- Sentinel state survives a pod restart, so a cluster reconverges on the master
  it had rather than the bootstrap master.
- Failover handling runs inline instead of in a detached goroutine, and
  re-queries Sentinel before issuing `SLAVEOF`, so a demotion cannot be issued
  from a stale address and discard the new master's acknowledged writes.
- NetworkPolicy selectors use the labels the operator actually applies.
- `RequeueAfter` is honoured, so status no longer goes stale between events.
- Metric series are deleted when their object is, so a deleted cluster stops
  reporting itself up and a scale-down stops reporting removed pods.
- Absolute counters read from `INFO` are advanced by their delta, so a Redis
  restart no longer looks like a burst to `rate()` and `increase()`.
- Failed backups are paced at 30 minutes instead of re-firing `BGSAVE` against
  the master in a tight loop.
- `spec.schedule`, `spec.retentionPolicy` and every cluster reference are
  validated.
- Documentation: the `redis.redguard.io/redis-name` selector, the metrics
  Service name, the plain-HTTP metrics instructions and the `clientCertRequired`
  and `podAntiAffinity` fields never existed.
- The published chart repository indexed 0.2.1 only, so the documented
  `--version 0.3.0` could not resolve and an unpinned install silently got the
  version that cannot start. 0.2.1 is now deprecated in the index and its
  tarball withdrawn; the release workflow publishes each tag's chart and then
  fails the release if the repository does not serve it.
- The documented upgrade order ran `helm upgrade` before the CRD apply. 0.2.1
  shipped no `redisrestores` CRD and the manager registers that controller
  unconditionally, so the new manager blocked on cache sync for about two
  minutes and exited, taking all four controllers down until the CRDs were
  applied.
- `docs/security.md` claimed `--watch-namespace` shrinks the operator's RBAC.
  It scopes the informer caches; the chart emits a ClusterRole either way, and
  isolation between tenants is one operator install per tenant.

### Known limitations

- **TLS clusters do not reach `Running`.** The Redis start-up script queries
  Sentinel without TLS flags, so on a TLS cluster it never resolves the master
  and the pod is killed by its liveness probe first. Separately, a `caSecretRef`
  naming a Secret other than `certificateSecretRef` produces a nested `subPath`
  mount that the container runtime refuses.
- **A restore can lose its own payload.** The restore raises Sentinel's
  `down-after-milliseconds` before restarting the master, but the `RedisSentinel`
  controller converges that value back towards the spec within one reconcile
  pass. If the restart outlasts the spec value plus an election, Sentinel
  promotes a replica and the restored dataset is discarded. Check that
  `status.phase` reached `Completed`.
- A single-replica cluster is not highly available; the operator reports
  `HighlyAvailable=False`.
- A Service cannot follow a failover while the operator is down.
- NetworkPolicy enforcement depends on the CNI.

## [0.2.1] - 2026-01-14

Chart packaging only, published to the Helm repository. The operator it installs
cannot start: the chart contained no ClusterRole for the manager, so every
reconcile failed with `Forbidden`. Deprecated as of 0.3.0 and its tarball
withdrawn from the repository, so `--version 0.2.1` fails on the download.

## [0.2.0] - 2026-01-09

### Added

- `RedisUser` for Redis 6+ ACL management: categories, commands, key patterns,
  channel patterns, password from a Secret.
- TLS fields on `RedisSentinel`: certificate and CA Secret references, mutual
  TLS.
- `RedisBackup` for S3 and S3-compatible backups: cron schedules, retention,
  gzip compression, IAM role support, one-time backups.
- Sample custom resources for the new kinds.

## [0.1.0] - 2026-01-09

### Added

- Initial release.
- `RedisSentinel` for a Redis master-replica set with Sentinel.
- StatefulSet deployment, persistent storage, ConfigMap-based configuration,
  probes and status reporting.
- Helm chart for the operator.
