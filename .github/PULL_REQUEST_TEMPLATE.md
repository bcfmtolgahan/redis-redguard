## What this changes

<!-- The failure mode fixed or the behaviour added. Link the issue if there is one. -->

## Checklist

- [ ] `go build ./... && go vet ./...` clean
- [ ] `make test` green
- [ ] `make verify-chart` passes. Ran `make manifests && make sync-chart` if an RBAC marker or an API type changed, and committed the result
- [ ] `make test-e2e` run, if a controller, the chart or an init script changed
- [ ] Behaviour change carries a test that fails without the change
- [ ] Commit subjects use a conventional prefix and no assistant attribution
