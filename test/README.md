# Tests

Beyond Go unit tests (`internal/provider/*_test.go`), the provider is
exercised against a real Kubernetes cluster using
[chainsaw](https://kyverno.github.io/chainsaw/). They apply OpenEverest
`Instance` CRs, assert on the operator-native resources the provider
produces, and verify status propagation back to the `Instance`.

There are two sibling suite directories, mirroring
[provider-percona-server-mongodb](https://github.com/openeverest/provider-percona-server-mongodb/tree/main/test):

- **`integration/`** — the provider's own translation and status logic.
  Deployed with the OpenSearch operator disabled (`deploy-provider-ci`);
  tests patch the `OpenSearchCluster` status directly to simulate operator
  readiness. Fast and deterministic.
- **`e2e-cluster/`** — the real OpenSearch operator (`deploy-provider-e2e`):
  an Instance must reach `Ready` against a genuinely running OpenSearch
  cluster, and the connection details the provider publishes are checked
  against the live cluster's REST API. Slower (a real OpenSearch pod has to
  start), but catches anything `integration`'s faked status can't — e.g. a
  CRD field the real operator rejects, or connection details that don't
  actually connect.

## Layout

```
test/
  vars.sh                      # Pinned operator/engine versions (sourced by make)
  integration/
    .chainsaw.yaml             # Shared chainsaw configuration (timeouts, reports)
    core/                      # Core lifecycle suite (create/update/delete)
      chainsaw-test.yaml       # Test definition: ordered steps
      00-assert.yaml           # Step files, prefixed by execution order
      ...
  e2e-cluster/
    .chainsaw.yaml             # Separate config: longer default timeouts
    core/                      # Create, wait for real Ready, verify connection, delete
      chainsaw-test.yaml
      ...
```

Each suite is a directory containing one `chainsaw-test.yaml` plus the
manifest/assert files its steps reference. Add more suites as sibling
directories under whichever of `integration/` or `e2e-cluster/` fits (e.g.
`integration/backup/`, `e2e-cluster/restore/`), and wire each one to its own
`test-integration-<suite>` or `test-e2e-cluster-<suite>` Make target and CI
job.

## Conventions

- **Numbered step files** — the `NN-` prefix mirrors the step order in
  `chainsaw-test.yaml`: `10-create-instance.yaml` applies a manifest,
  `10-assert.yaml` asserts the expected outcome of that step.
- **Simulate the operator, except where it matters** — `integration/`
  suites deploy with the OpenSearch operator disabled (see
  `deploy-provider-ci` in the Makefile) and patch the operator-native CR
  status (`kubectl patch --subresource status`) to simulate readiness. This
  keeps them fast and deterministic: you are testing the *provider's*
  translation and status logic, not the operator itself. Keep at least one
  suite in `e2e-cluster/` against the real operator (`deploy-provider-e2e`)
  so a CRD shape the real operator actually rejects, or connection details
  that don't actually connect, don't slip through unnoticed.
- **Assert both directions** — after applying an `Instance`, assert the
  operator-native CR spec the provider must produce; after the operator
  (real or simulated) reports readiness, assert the `Instance` reaches
  `phase: Ready`.
- **Collect diagnostics on failure** — use the `catch` block in
  `chainsaw-test.yaml` to dump relevant resources and provider logs.

## Running locally

Prerequisites: a running cluster with the OpenEverest CRDs, your operator's
CRDs, the OpenEverest controller, and the provider deployed (see the
`install-crds`, `deploy-provider-ci`/`deploy-provider-e2e` Make targets),
plus the [chainsaw CLI](https://kyverno.github.io/chainsaw/latest/quick-start/install/):

```bash
go install github.com/kyverno/chainsaw@latest
```

Then:

```bash
make test-integration        # all integration suites (deploy-provider-ci)
make test-integration-core   # just the core integration suite
make test-e2e-cluster        # all e2e-cluster suites (deploy-provider-e2e)
make test-e2e-cluster-core   # just the core e2e-cluster suite
```

## Running in CI

`.github/workflows/ci.yaml` runs each suite as a separate job through one of
two reusable workflows:

- `.github/workflows/integration-test.yaml` for `test/integration/` suites
  (deploys with `deploy-provider-ci`)
- `.github/workflows/e2e-cluster-test.yaml` for `test/e2e-cluster/` suites
  (deploys with `deploy-provider-e2e`, the real operator)

Both provision a k3d cluster, build and import the provider image, build and
deploy the OpenEverest controller, and invoke the suite's Make target. To add
a suite to CI, add a job to `ci.yaml` using the workflow that matches where
the suite lives.
