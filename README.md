# Redguard

Redguard is a Kubernetes operator that runs Redis in a master-replica set
supervised by Redis Sentinel, and moves the write endpoint when Sentinel
promotes a new master.

You declare a `RedisSentinel`; the operator creates the StatefulSets, the
Services, the configuration, the PodDisruptionBudgets and the NetworkPolicies,
watches Sentinel, and keeps the `<name>-redis` Service pointing at whichever pod
is master right now.

## What it does

- Redis master-replica sets with Sentinel-driven failover.
- A write Service that selects only the master, and a separate Service for
  read-only traffic.
- Durable Sentinel state on a PVC, so a restarted Sentinel keeps the master it
  learned instead of re-seeding from the template.
- Authentication for both Redis and Sentinel from one Secret, rotated online
  without dropping replication.
- Declarative ACL users (`RedisUser`) confined to their declared key, channel
  and command scope.
- Scheduled and one-off RDB backups to S3 or an S3-compatible endpoint
  (`RedisBackup`), and restore from one (`RedisRestore`).
- Prometheus metrics per cluster, per pod and per backup.

## What it does not do

- It does not run Redis Cluster (sharding). One `RedisSentinel` is one dataset.
- It does not create or renew certificates. See the TLS limitation below.
- It does not manage clients. A client that caches the master address across a
  failover has to reconnect; use the Service or a Sentinel-aware client.
- It does not move the write endpoint while the operator itself is down. Only a
  controller can make a Service follow a Sentinel promotion.

## Requirements

- Kubernetes 1.33 or newer. The e2e suite runs against 1.33, 1.34 and 1.36.
- A default StorageClass, or one named in `spec.redisConfig.storage`.
- `kubectl`, and `helm` 3 for the chart install.

## Install

With Helm:

```sh
helm repo add redguard https://bcfmtolgahan.github.io/redis-redguard
helm repo update
helm install redguard redguard/redguard \
  --version 0.3.0 \
  --namespace redguard-system --create-namespace \
  --wait
```

Pin `--version`. 0.3.0 is the first release that starts; 0.2.1 is deprecated in
the index and its tarball withdrawn, because its chart never granted the
operator the RBAC it needs.

The repository is GitHub Pages serving `docs/`, and the release workflow
publishes into it from a `v*` tag, so a version resolves only once its tag is
released. `helm repo update` first if `--version` reports `no chart version
found`. Upgrades of an existing install need the CRDs applied before
`helm upgrade`; the ordering and what breaks without it are in
[docs/install.md](docs/install.md#upgrade).

With plain manifests:

```sh
kubectl apply -f https://github.com/bcfmtolgahan/redis-redguard/releases/download/v0.3.0/install.yaml
```

Check it:

```sh
kubectl -n redguard-system get deploy
kubectl -n redguard-system logs -l control-plane=controller-manager | head
```

The Deployment is `redguard` after the chart install and
`redguard-controller-manager` after the manifest install; the label above
selects either.

Chart options, including `operator.watchNamespaces` and the backup destination
allowlist, are in [docs/install.md](docs/install.md). To build and run from a
checkout, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Quick start

```sh
kubectl apply -f - <<'EOF'
apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: redis-cluster
spec:
  redisConfig:
    replicas: 3
    storage:
      size: 1Gi
  sentinelConfig:
    replicas: 3
    quorum: 2
EOF
```

Wait for it:

```sh
kubectl get redissentinel redis-cluster -w
```

```
NAME            PHASE     MASTER             REPLICAS   SENTINELS   AGE
redis-cluster   Running   10.244.0.10:6379   3          3           45s
```

Write through the master Service and read through the replicas Service:

```sh
kubectl run redis-client --rm -i --restart=Never --image redis:7-alpine -- \
  redis-cli -h redis-cluster-redis SET greeting hello

kubectl run redis-client --rm -i --restart=Never --image redis:7-alpine -- \
  redis-cli -h redis-cluster-redis-replicas GET greeting
```

The master carries a label; that is how the write Service finds it:

```sh
kubectl get pods -l app.kubernetes.io/instance=redis-cluster,redis.redguard.io/role=master
```

## What gets created

For a `RedisSentinel` named `redis-cluster`:

| Object | Name | Purpose |
| --- | --- | --- |
| StatefulSet | `redis-cluster-redis` | Redis pods, one PVC each |
| StatefulSet | `redis-cluster-sentinel` | Sentinel pods, one PVC each for durable state |
| Service | `redis-cluster-redis` | Writes. Selects the master pod only |
| Service | `redis-cluster-redis-replicas` | Reads. Selects every Redis pod |
| Service | `redis-cluster-redis-headless` | Stable per-pod DNS for replication |
| Service | `redis-cluster-sentinel` | Sentinel-aware clients, port 26379 |
| Service | `redis-cluster-sentinel-headless` | Stable per-pod DNS for the quorum |
| ConfigMap | `redis-cluster-redis-config` | `redis.conf` and the init script |
| ConfigMap | `redis-cluster-sentinel-config` | `sentinel.conf` and the init script |
| PodDisruptionBudget | `redis-cluster-redis-pdb`, `redis-cluster-sentinel-pdb` | `maxUnavailable: 1` per component |
| NetworkPolicy | `redis-cluster-redis-netpol`, `redis-cluster-sentinel-netpol` | Ingress from the workload namespace and the operator namespace |

Every object carries `app.kubernetes.io/instance: redis-cluster` and
`app.kubernetes.io/component: redis` or `sentinel`. There is no
`redis.redguard.io/redis-name` label; select on `app.kubernetes.io/instance`.

The Sentinel monitor name is `<cluster>-master`, for example
`redis-cluster-master`.

## Custom resources

| Kind | Purpose |
| --- | --- |
| `RedisSentinel` | One Redis master-replica set with its Sentinels |
| `RedisUser` | One Redis ACL user on a cluster, applied to every node |
| `RedisBackup` | Scheduled or one-off RDB backup to S3 |
| `RedisRestore` | Load an S3 backup back into a cluster |

Every field is in [docs/reference.md](docs/reference.md).

## Documentation

- [docs/install.md](docs/install.md) — chart values, operator flags, upgrade
  and uninstall.
- [docs/reference.md](docs/reference.md) — the four custom resources, field by
  field, with their status and conditions.
- [docs/operations.md](docs/operations.md) — failover, scaling, configuration
  changes, password rotation, backup, restore, metrics and alerts,
  troubleshooting.
- [docs/security.md](docs/security.md) — threat model, what the ACL allowlist
  confines, the backup destination allowlist, operator RBAC, Pod Security,
  NetworkPolicy.
- [CHANGELOG.md](CHANGELOG.md) — releases, including the 0.2.1 breaking changes.

## Limitations

- **A single-replica cluster is not highly available.** `replicas: 1` is legal
  and useful for development, but Sentinel has nothing to promote. The operator
  reports it: `HighlyAvailable=False`, reason `NoFailoverTarget`.
- **The write Service is only as current as the operator.** Sentinel promotes a
  replica on its own, but the role label that steers `<name>-redis` moves only
  when the operator reconciles. While the operator is down the Service keeps
  selecting the demoted pod.
- **There is a write gap during a failover.** Between the master failing and the
  label moving, `<name>-redis` has no endpoints and writes fail fast, which is
  the intent: the alternative is a write silently accepted by a read-only
  replica.
- **NetworkPolicies need a CNI that enforces them.** The operator writes the
  objects regardless. Under a CNI that ignores NetworkPolicy they are inert.
- **TLS does not currently work.** `spec.tls` is accepted and the pods are
  configured for it, but the Redis start-up script queries Sentinel without TLS
  and never resolves the master, so the Redis pods do not become ready. See
  [docs/operations.md](docs/operations.md#tls).
- **A restore restarts the master.** If the restart outlasts
  `sentinelConfig.downAfterMilliseconds`, Sentinel can promote a replica and the
  restored dataset is discarded. Check `status.phase` on the `RedisRestore`.

## Uninstall

```sh
kubectl delete redissentinel --all --all-namespaces
helm uninstall redguard -n redguard-system
kubectl delete namespace redguard-system
```

Helm does not delete CRDs. Removing them deletes every remaining custom
resource in the cluster:

```sh
kubectl delete crd \
  redissentinels.redis.redguard.io \
  redisusers.redis.redguard.io \
  redisbackups.redis.redguard.io \
  redisrestores.redis.redguard.io
```

PersistentVolumeClaims outlive their StatefulSet. Delete them separately once
you no longer need the data.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities through the
process in [SECURITY.md](SECURITY.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
