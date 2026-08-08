# Contributing to Redguard

Participation is covered by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Vulnerabilities go through the private route in [SECURITY.md](SECURITY.md), not through a public issue.

## Prerequisites

- Go 1.24.6 or newer. `go.mod` sets the language version and CI reads it from there.
- Docker, running. The e2e suite builds the operator image. `CONTAINER_TOOL=podman` also works.
- kind, for the e2e suite.
- Helm 3 and kubectl.

`controller-gen`, `kustomize` and `setup-envtest` are downloaded into `bin/` on first use at the versions pinned in the Makefile. Do not install them globally.

## Build and test

```bash
go build ./... && go vet ./...
make test          # unit and envtest suites, about a minute
make verify-chart  # chart drift and rendered-deployment gate
make test-e2e      # kind, 6-8 minutes
```

`make test-e2e` builds the image, creates a kind cluster with a name unique to the run, and drives it through a kubeconfig of its own. It never reads or writes `~/.kube/config`, and it deletes only a cluster it created itself. Do not point it at an existing cluster.

## The Helm chart is generated

`charts/redguard/crds/` and the RBAC templates (`role.yaml`, `rolebinding.yaml`, `leader-election-*`, `metrics-auth-*`) are produced from `config/` by `hack/sync-chart.sh`. Editing them by hand is how the chart and the operator's real RBAC drift apart. After changing a kubebuilder marker or an API type:

```bash
make manifests
make sync-chart
git add config charts
```

`make verify-chart` fails on any drift between `config/` and the chart, and additionally asserts that the rendered deployment keeps `--leader-elect`, production logging and the 60s termination grace period. It runs in CI, so an unsynced chart fails the build.

## Run the operator against kind

Give the cluster a kubeconfig of its own, so nothing you run here can reach a cluster you did not create.

```bash
kind create cluster --name redguard-dev --kubeconfig /tmp/redguard-dev.kubeconfig
export KUBECONFIG=/tmp/redguard-dev.kubeconfig

make install   # CRDs only
make run       # operator on your host, against the kind cluster

kubectl apply -f config/samples/redis_v1alpha1_redissentinel.yaml
kubectl get redissentinel redis-cluster -w
```

`make run` passes no flags. Run the binary directly to exercise flag-dependent behaviour:

```bash
go run ./cmd/main.go --watch-namespace=default --metrics-bind-address=:8080
```

From the host the operator reconciles StatefulSets, Services and ConfigMaps normally, but it dials Redis and Sentinel on pod IPs, which the kind pod network does not expose to the host. A cluster started this way stays in `Creating`. To work on anything that talks to Redis, put the operator in the cluster:

```bash
make docker-build IMG=redguard:dev
kind load docker-image redguard:dev --name redguard-dev
helm upgrade --install redguard charts/redguard \
  --namespace redguard-system --create-namespace \
  --set operator.image.repository=redguard \
  --set operator.image.tag=dev \
  --set operator.image.pullPolicy=Never \
  --wait
kubectl logs -n redguard-system deploy/redguard -f
```

Tear down the whole cluster when you are done:

```bash
kind delete cluster --name redguard-dev --kubeconfig /tmp/redguard-dev.kubeconfig
```

## Tests

Behaviour changes come with a test that fails before the change and passes after it. Controller tests run against envtest with a fake Redis client (`internal/redisclient/fake`); the init scripts are executed under `sh` with stub binaries in `internal/builder/testdata`. Reach for the e2e suite only for behaviour that needs a real Redis and a real failover.

## Commits

- Conventional prefix: `feat:`, `fix:`, `docs:`, `test:`, `chore:`, `ci:`, `refactor:`.
- Subject in the imperative, under 72 characters. The body explains the failure mode fixed, not the diff.
- No `Co-Authored-By` or assistant attribution trailers.
- One logical change per commit. The tree builds and `make test` is green at every commit.

## Pull requests

Run before opening one:

```bash
go build ./... && go vet ./...
make test
make verify-chart
```

Add `make test-e2e` if you touched a controller, the chart, or the init scripts.

## Style

- Plain technical English. No emoji, no banners, no marketing tone.
- Comments carry the constraint, the invariant, or the non-obvious reason, in one or two lines. They do not restate the code or narrate history.
- Shell embedded in ConfigMaps is POSIX `sh` (busybox ash in `redis:7-alpine`). No bashisms.
- Never write a password to a log line or to stdout, in Go or in shell.

## License

Contributions are licensed under the Apache License 2.0, the terms in [LICENSE](LICENSE). New Go files carry the same header as the existing ones.

