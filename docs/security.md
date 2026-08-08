# Security

## Threat model

Redguard assumes a multi-tenant cluster. Namespace owners create
`RedisSentinel`, `RedisUser`, `RedisBackup` and `RedisRestore` objects; the
operator holds cluster-wide permissions and an AWS identity that they do not.
The interesting question is therefore what a namespace owner can make the
operator do on their behalf.

What the operator defends against:

- **A `RedisUser` escaping its declared scope.** ACL grants are checked against
  an allowlist and narrowed by a confinement floor, so a user cannot enumerate
  or wipe the keyspace, or reach administrative commands.
- **A `RedisUser` hijacking the admin account.** `default` is reserved.
- **A `RedisBackup` pointing the operator's AWS identity somewhere else.**
  IAM-role backups must name a bucket and endpoint the cluster administrator
  allowlisted on the operator.
- **Custom configuration overriding what the operator owns.** Directives that
  set authentication, listening, storage location or replication are rejected,
  and no `customConfig` value can inject a second directive.
- **Retention deleting objects it did not write.** Pruning is confined to a
  prefix that always contains the namespace and cluster name, and only to
  objects named `backup-*.rdb` or `backup-*.rdb.gz`.
- **Two operators fighting.** Leader election is on by default; the chart
  refuses more than one replica without it.

What it does not defend against:

- **Anyone who can reach the Redis port in the workload namespace.** The
  generated NetworkPolicy admits the whole namespace, because clients carry no
  label the operator can predict. Enforce your own policy if that is too wide.
- **A cluster without authentication.** `spec.redisConfig.auth` is optional. With
  it unset, the `default` user is `nopass` and anything that reaches port 6379
  has full access.
- **Traffic interception.** TLS does not currently work; see
  [operations.md](operations.md#tls).
- **Anyone with `update` on `RedisSentinel` objects in a namespace.** They pick
  the image, the resource requests, the `priorityClassName` and the
  `tolerations`, so they can place a pod of their choosing on a tainted node the
  namespace would not otherwise reach. Gate the CRDs with your own RBAC, and use
  a ResourceQuota and a scheduling admission policy if the namespace is not
  trusted.
- **A malicious operator image.** Pin the image by digest if that matters to you.

## What the ACL allowlist confines

A denylist would not work: Redis adds commands over time and every future one
would default to permitted, and the escape routes are open-ended (`KEYS`,
`SCAN`, `FLUSHALL`, `MONITOR`, `PSYNC`, `MIGRATE`, ...). Grants are therefore
allowlisted.

**Command grants.** `+cmd` is accepted only for a command that carries a key
specification — so a `~pattern` confines it — or that touches neither keys nor
channels. The set is derived from the `COMMAND` table of the pinned Redis image.
Anything else is refused at reconcile:

```
aclRules: rule "+keys" is not permitted: keys has no key specification and runs
across the whole keyspace or the server, escaping the user's scope; grant a
scoped category such as +@read or a key-scoped command such as +get instead
```

**Category grants.** `+@category` is accepted for any category Redis defines
except `@all`, `@admin` and `@dangerous`.

**Standalone keywords.** `nopass`, `reset`, `resetpass`, `on`, `off` and
`allcommands` are refused: they weaken authentication or fight the fields the
operator renders itself. `allkeys`, `resetkeys`, `allchannels`,
`resetchannels`, `nocommands`, `clearselectors`, `sanitize-payload` and
`nosanitize-payload` are allowed — none widens access beyond the declared scope.

**Passwords.** Rules beginning `>`, `<`, `#` or `!` are refused. The password
comes from `passwordSecretRef`.

**Selectors.** Redis 7 selector syntax `(...)`, and any rule containing
whitespace, is refused at admission.

**The confinement floor.** A category still bundles commands that carry no key
specification and therefore ignore `~patterns`. `@read` pulls in `KEYS`, `SCAN`
and `RANDOMKEY`; `@write` pulls in `FLUSHALL`, `FLUSHDB` and `SWAPDB`. So when a
user grants any category or command, the operator appends, after the user's own
rules:

```
-@admin -@dangerous -scan -randomkey -dbsize -pubsub -function -script
```

Redis applies rules left to right, so these trailing removals win. `-pubsub`
drops the channel-name enumerators while leaving `SUBSCRIBE` and `PUBLISH`;
`-function` and `-script` drop server-global state management while leaving the
key-scoped `EVAL` and `FCALL`. A user that grants nothing gets no floor, because
there is nothing to confine.

The rendered account for the sample user is:

```
user app-user on sanitize-payload #<sha256> ~app:* ~cache:* resetchannels
&notifications:* -@all +@read +@write +get +set +del -flushdb -flushall
-@admin -@dangerous -scan -randomkey -dbsize -pubsub -function -script
```

and it behaves as declared:

```
SET app:k1 v1     -> OK
SET other:k1 v1   -> NOPERM No permissions to access a key
SCAN 0            -> NOPERM User app-user has no permissions to run the 'scan' command
FLUSHALL          -> NOPERM User app-user has no permissions to run the 'flushall' command
```

Nothing is granted implicitly. Every rule list opens with `reset`, so the
account ends up exactly as the spec describes and a rule removed from the spec
is removed from Redis.

## The reserved admin account

`spec.username: default` is rejected at admission and again at reconcile. The
`default` user is the account whose password is `requirepass`; redefining it
would reset the cluster password and lock the operator out.

The `default` user line in each node's ACL file is rebuilt from the current
password on every start, so a rotated password takes effect and an ACL file that
lost the line cannot silently leave the instance open. Only the SHA-256 of the
password is stored, and it reaches `sha256sum` through a pipe, never through
`argv`.

## Configuration the operator owns

`customConfig` cannot override the directives that decide authentication,
listening, storage location or replication. Keys and values may not contain line
breaks, which is what keeps the rendered `<key> <value>` line injection-proof.
The rejected sets are listed in [reference.md](reference.md#specredisconfig).

## The backup destination allowlist

A `RedisBackup` with `spec.s3.credentialsSecretRef` signs with a Secret the
namespace owner already holds. It is not restricted: it can only reach what
those credentials could reach anyway.

A `RedisBackup` with `spec.s3.useIAMRole` signs with the **operator's** AWS
identity, which is usually broader than the namespace owner's. Those requests
must pass a destination policy set by whoever installed the operator:

- `--allowed-backup-buckets` — buckets an IAM-role backup may target. Empty
  disables the IAM-role path entirely.
- `--allowed-backup-endpoints` — custom endpoints it may target. Empty permits
  only the default AWS endpoint, because a custom endpoint would redirect a
  request signed with the operator's identity to a host of the requester's
  choosing.

The check runs both at reconcile and immediately before the S3 client is built,
so no future call site can reach AWS with the operator's identity without
passing it. A refusal is reported distinctly from a failed run:
`Ready=False`, reason `DestinationNotAllowed`.

Admission also rejects a spec with neither credential source, because the AWS
SDK would then sign with whatever ambient identity the operator pod carries and
never pass the allowlist at all.

## Operator RBAC

The ClusterRole is generated from the controllers' kubebuilder markers
(`config/rbac/role.yaml`) and the chart consumes it; it is never restated by
hand. It grants, cluster-wide:

| Group | Resources | Verbs |
| --- | --- | --- |
| `redis.redguard.io` | the four kinds, their `/status` and `/finalizers` | full |
| core | `configmaps`, `services` | full |
| core | `pods` | get, list, watch, update, patch |
| core | `pods/exec` | create |
| core | `secrets` | create, get, list, update, watch |
| core | `events` | create, patch |
| apps | `statefulsets` | full |
| batch | `jobs` | full |
| networking.k8s.io | `networkpolicies` | full |
| policy | `poddisruptionbudgets` | full |

Two grants are worth calling out. `pods/exec` is how a backup reads
`/data/dump.rdb` out of the master and how a restore stages the payload; it is
equivalent to a shell in any Redis pod the operator manages. `secrets` is read
across every watched namespace, because the auth password, the TLS certificates
and the S3 credentials are user-created and carry no operator label.

Neither shrinks with `--watch-namespace`. The chart emits a ClusterRole and a
ClusterRoleBinding whatever the value is; the flag scopes the manager's
informer caches, so the operator lists, watches and reconciles only the named
namespaces, but its ServiceAccount still holds `secrets` and `pods/exec`
everywhere. Anyone who can exec into the operator pod, or read its
ServiceAccount token, reaches every namespace in the cluster.

So isolation is per install, not per namespace. To keep one tenant's operator
away from another tenant's Secrets, run one release per tenant:

```sh
helm install redguard-team-a redguard/redguard \
  --namespace redguard-team-a --create-namespace \
  --set 'operator.watchNamespaces={team-a}'
```

Each release gets its own ServiceAccount and its own name-prefixed ClusterRole,
and reconciles only its tenant's namespaces. Give each one a namespace of its
own: the leader-election lease name is fixed, so two releases sharing a
namespace would elect against each other and only one would run.

This bounds what an operator *does*, not what its token *could* do: every one
of those ClusterRoles is still cluster-wide.

Narrowing the permission itself means binding the generated ClusterRole with a
RoleBinding in each watched namespace instead of a ClusterRoleBinding. The
chart does not do this, and `rbac.create=false` drops every RBAC object it
renders, so all of them become yours to write.

Leader election adds a namespaced Role for `configmaps`, `leases` and `events`
in the operator's own namespace. Metrics authentication adds a ClusterRole for
creating `tokenreviews` and `subjectaccessreviews`; those are cluster-scoped
resources, so that one cannot be narrowed to a namespace at all.

## Pod Security

The operator and the pods it creates satisfy the **restricted** Pod Security
Standard. A namespace labelled
`pod-security.kubernetes.io/enforce=restricted` runs a cluster unchanged.

Redis and Sentinel pods run as uid and gid 1000 with `fsGroup` 1000 — `fsGroup`
is what makes the data volume and the mounted TLS material readable, so the
`redis` uid baked into the image would buy nothing. They also set
`runAsNonRoot`, `seccompProfile: RuntimeDefault`,
`allowPrivilegeEscalation: false`, all capabilities dropped, and a read-only
root filesystem with a writable `emptyDir` at `/tmp`.

The operator container is the same posture at uid and gid 65532, without the
`/tmp` volume: it writes nothing to disk.

## NetworkPolicy

The operator writes one NetworkPolicy per component, with both `Ingress` and
`Egress`:

- Ingress on the client port from **every pod in the workload namespace**.
  Clients carry no label the operator can predict, and Sentinel and the replicas
  live there too.
- Ingress from the operator's own namespace, selected by
  `kubernetes.io/metadata.name`. Without it the operator cannot reach the
  clusters it manages and every status field goes stale.
- Egress to DNS, to the cluster's own Redis pods (replication) and to its own
  Sentinel pods.

Everything else is denied — including other namespaces, and including egress to
the internet from a Redis pod.

Two caveats. The policy is only as strong as the CNI: under a CNI that does not
implement NetworkPolicy the objects exist and enforce nothing. And admitting the
whole workload namespace means any pod there can reach Redis; put the cluster in
its own namespace if that matters.

## Secrets and logging

Passwords are never written to stdout, stderr or a log line, in Go or in shell.
The admin password reaches the start-up script through the environment and is
substituted with `index`/`substr` rather than `gsub`, so `&` and `\` in a
password stay literal. The generated ACL file is `chmod 600`. The Sentinel state
file, which contains the credential lines, is never readable beyond its owner.

The operator keeps the password the cluster currently accepts in a Secret named
`<cluster>-auth-state`, owned by the `RedisSentinel` and garbage collected with
it. It exists because during a rotation the user's Secret already holds the new
password while every node still requires the previous one.

## The metrics endpoint

`/metrics` is served over HTTPS with token authentication and a
SubjectAccessReview authorization check, on port 8080 by default. It is not
plain HTTP, and an unauthenticated scrape is rejected. Granting a scraper access
is in [operations.md](operations.md#metrics).

With no certificate configured, controller-runtime generates a self-signed one.
Point `--metrics-cert-path` at a real certificate for production scraping.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md).
