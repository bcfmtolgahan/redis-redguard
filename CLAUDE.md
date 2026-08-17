# Working in this repository

A Kubernetes operator for Redis with Sentinel. Go, kubebuilder layout, module
`github.com/bcfmtolgahan/redis-redguard`.

## Generated files

`charts/redguard/crds/`, `charts/redguard/templates/role.yaml` and the
leader-election and metrics-auth templates are generated from `config/` by
`hack/sync-chart.sh`. Never edit them by hand. Change the kubebuilder markers or
the API types, then:

```sh
make manifests && make sync-chart
```

`make verify-chart` fails on drift and runs in CI. The 0.2.1 release shipped a
hand-written ClusterRole that had fallen six rule groups behind the markers, so
the operator could not sync its caches and never started. That is why this is
generated.

Two other values are derived rather than pinned, and both broke a build when they
were not: the builder image's Go version comes from go.mod's directive, and
`ENVTEST_K8S_VERSION` comes from the `k8s.io/api` version.

## Tests

```sh
make test        # unit + envtest, about a minute
make lint        # must be clean; CI runs the same linter
make test-e2e    # kind, 7-15 minutes, builds an image
```

`make test-e2e` creates a kind cluster of its own with its own kubeconfig and
refuses to touch a cluster it did not create. Do not weaken that: the shared
kubeconfig on the maintainer's machine holds several live production clusters,
and an earlier revision of the harness aimed `helm uninstall` at one of them.

envtest binaries live in `bin/k8s`. To skip the Makefile's resolution step:

```sh
export KUBEBUILDER_ASSETS="$(pwd)/bin/k8s/1.34.1-darwin-arm64"
```

## What the tests are for

Two classes of defect dominate this codebase's history, and the suite is shaped
around them.

The generated shell in `internal/builder/configmap.go` is executed by tests under
`sh` and busybox, not asserted as strings. Both fatal boot bugs in 0.2.1 lived
there and neither was visible by reading.

Anything that touches Redis goes through `internal/redisclient`, whose fake
records calls per address. Controllers must obtain clients from `r.RedisFactory`;
a guard test fails the build if one constructs a client directly, because a
direct client cannot be tested and silently ignores TLS.

## Invariants that cost data when broken

- A pub/sub event from Sentinel is a trigger, never a source of truth. The
  reconcile it wakes re-queries Sentinel before moving the master label or
  issuing `SLAVEOF`. Acting on an event's own view of the master once lost
  acknowledged writes.
- Nothing demotes a pod claiming to be master outside the failover path, which
  re-verifies with Sentinel first and abandons the pass on a mismatch.
- No path destroys or overwrites a dataset before the replacement is confirmed
  present and readable.
- Polling is load-bearing. The 30s steady and 5s failover requeues converge the
  cluster when the subscription is down; do not lengthen them or treat the
  subscription as a replacement.

## Commits

Conventional prefix, subject under 72 characters, body explaining the failure
mode being fixed rather than the change being made. No `Co-Authored-By` or
assistant attribution of any kind.

Comments state a constraint, an invariant, or a non-obvious "why", in a line or
two. They do not restate the next line, narrate the change, or reference a
review. No emoji anywhere, including docs and issue templates.
