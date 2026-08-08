# Installing and configuring the operator

## Install

```sh
helm repo add redguard https://bcfmtolgahan.github.io/redis-redguard
helm repo update
helm install redguard redguard/redguard \
  --version 0.3.0 \
  --namespace redguard-system --create-namespace \
  --wait
```

Or, without Helm:

```sh
kubectl apply -f https://github.com/bcfmtolgahan/redis-redguard/releases/download/v0.3.0/install.yaml
```

The manifest carries the same CRDs and the same ClusterRole, but it is rendered
from `config/` rather than from the chart, so the object names and the metrics
port differ:

| | Helm | Manifest |
| --- | --- | --- |
| Deployment | `redguard` | `redguard-controller-manager` |
| Metrics Service | `redguard-metrics` on 8080 | `redguard-controller-manager-metrics-service` on 8443 |
| Namespace | whatever `--namespace` says | `redguard-system`, fixed |

It cannot be configured further. Use the chart if you need any of the values
below.

## Chart values

| Value | Default | Effect |
| --- | --- | --- |
| `operator.replicas` | `1` | Deployment replicas. More than one requires `operator.leaderElect`; the chart refuses the combination otherwise. |
| `operator.leaderElect` | `true` | Passes `--leader-elect`. Two active managers race on master role labels and StatefulSet updates. |
| `operator.watchNamespaces` | `[]` | Namespaces the operator watches, joined into `--watch-namespace`. Empty watches the whole cluster. |
| `operator.terminationGracePeriodSeconds` | `60` | Must stay above the manager's 30s reconcile drain plus the leader lease handover. |
| `operator.devLogging` | `false` | Passes `--zap-devel`: console output at debug level. The default is JSON at info level. |
| `operator.image.repository` / `.pullPolicy` | `ghcr.io/bcfmtolgahan/redguard` / `IfNotPresent` | Operator image. |
| `operator.image.tag` | the release's own tag | Set it empty to fall back to the chart `appVersion`. |
| `operator.resources` | 500m / 512Mi limit | The informer cache dominates memory. Set `operator.watchNamespaces` before lowering it. |
| `operator.podSecurityContext`, `operator.containerSecurityContext` | restricted-compliant | Non-root uid 65532, read-only root filesystem, all capabilities dropped. |
| `operator.nodeSelector`, `.tolerations`, `.affinity` | `{}` / `[]` | Scheduling of the operator pod itself. |
| `backup.allowedBuckets` | `[]` | S3 buckets a `RedisBackup` may target with `spec.s3.useIAMRole`. Empty disables the IAM-role path. |
| `backup.allowedEndpoints` | `[]` | Custom S3 endpoints permitted for IAM-role backups. Empty permits only the default AWS endpoint. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | Annotate it to attach an IRSA role for IAM-role backups. |
| `rbac.create` | `true` | Creates the ClusterRole, the leader-election Role and their bindings. |
| `metrics.enabled` / `metrics.port` | `true` / `8080` | Metrics listener and the `<release>-metrics` Service. The endpoint is HTTPS with authn and authz. |
| `health.port` | `8081` | Liveness and readiness probes. |

## Scoping the operator to namespaces

An unscoped operator keeps a cluster-wide informer cache of every Pod,
ConfigMap, Service, StatefulSet, NetworkPolicy and PodDisruptionBudget it
manages, plus every Secret in every namespace. Secrets cannot be filtered by
label, because the auth password, the TLS certificates and the S3 credentials
belong to the user and carry no operator label.

```sh
helm upgrade --install redguard redguard/redguard \
  --namespace redguard-system \
  --set 'operator.watchNamespaces={team-a,team-b}'
```

The operator then sees only those namespaces and acts only in them. Custom
resources in any other namespace are ignored.

## Allowing IAM-role backups

A `RedisBackup` with `spec.s3.useIAMRole` runs under the operator's own AWS
identity. The destination must be allowlisted on the operator, or the backup is
refused with `DestinationNotAllowed`:

```sh
helm upgrade --install redguard redguard/redguard \
  --namespace redguard-system \
  --set 'backup.allowedBuckets={acme-redis-backups}' \
  --set 'serviceAccount.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::111122223333:role/redguard-backup'
```

Backups that supply `spec.s3.credentialsSecretRef` are not restricted: they sign
with a Secret the namespace owner already holds.

## Operator flags

The chart sets these from its values. They are listed here for the manifest
install and for `make run`.

| Flag | Default | Effect |
| --- | --- | --- |
| `--leader-elect` | off | Single active manager. |
| `--watch-namespace` | empty | Comma-separated namespaces to watch. Empty is cluster-wide. |
| `--metrics-bind-address` | `0` | `0` disables metrics. The chart sets `:8080`. |
| `--metrics-secure` | `true` | HTTPS with token authn and SubjectAccessReview authz. |
| `--health-probe-bind-address` | `:8081` | Serves `/healthz` and `/readyz`. |
| `--allowed-backup-buckets` | empty | Buckets reachable with the operator's IAM role. |
| `--allowed-backup-endpoints` | empty | Custom S3 endpoints reachable with the operator's IAM role. |
| `--enable-http2` | `false` | HTTP/2 on the metrics and webhook servers. |
| `--zap-devel` | `false` | Console logging at debug level. |

## Upgrade

```sh
helm repo update
helm upgrade redguard redguard/redguard --version 0.3.0 -n redguard-system
```

Helm installs the CRDs from the chart's `crds/` directory on `helm install` and
never touches them again. Apply them yourself when a release changes the API:

```sh
helm pull redguard/redguard --version 0.3.0 --untar --untardir /tmp
kubectl apply -f /tmp/redguard/crds/
```

Do not run the release `install.yaml` against a Helm installation to update the
CRDs: it also carries a Deployment of its own and would leave two operators
reconciling the same clusters.

Upgrading the operator restarts Redis whenever the new version changes the pod
template or the generated configuration, which a release usually does. Expect a
rolling restart of every managed cluster, and with it at least one failover per
cluster. Roll one namespace at a time with `operator.watchNamespaces` if that
matters.

## Uninstall

```sh
kubectl delete redissentinel --all --all-namespaces
helm uninstall redguard -n redguard-system
kubectl delete namespace redguard-system
kubectl delete crd \
  redissentinels.redis.redguard.io \
  redisusers.redis.redguard.io \
  redisbackups.redis.redguard.io \
  redisrestores.redis.redguard.io
```

Delete the `RedisSentinel` objects before the operator. Each one carries the
`redis.redguard.io/finalizer` finalizer, and only the operator removes it: a CRD
deleted while an instance still exists stays in `Terminating` until someone
strips the finalizer by hand.

PersistentVolumeClaims are not deleted with the StatefulSet. Remove them when
the data is no longer needed:

```sh
kubectl delete pvc -l app.kubernetes.io/instance=redis-cluster
```
