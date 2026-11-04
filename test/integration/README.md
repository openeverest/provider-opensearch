# Integration tests

Integration tests exercise the provider against a real Kubernetes cluster
using [chainsaw](https://kyverno.github.io/chainsaw/). They apply OpenEverest
`Instance` CRs, assert on the operator-native resources the provider produces,
and verify status propagation back to the `Instance`.

There are two suites:

- **`core/`** — the provider's own translation and status logic. Deployed
  with the OpenSearch operator disabled (`deploy-provider-ci`); tests patch
  the `OpenSearchCluster` status directly to simulate operator readiness.
  Fast and deterministic.
- **`e2e-cluster/`** — the real OpenSearch operator (`deploy-provider-e2e`):
  an Instance must reach `Ready` against a genuinely running OpenSearch
  cluster, and the connection details the provider publishes are checked
  against the live cluster's REST API. Slower (a real OpenSearch pod has to
  start), but catches anything `core`'s faked status can't — e.g. a CRD
  field the real operator rejects, or connection details that don't actually
  connect.

For a complete, real-world example of a larger suite set see the
[provider-percona-server-mongodb test suites](https://github.com/openeverest/provider-percona-server-mongodb/tree/main/test/integration).

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
    e2e-cluster/                # Real-operator suite (create, wait for real Ready, verify connection, delete)
      chainsaw-test.yaml
      ...
```

Each suite is a directory containing one `chainsaw-test.yaml` plus the
manifest/assert files its steps reference. Add more suites as sibling
directories (e.g. `backup/`, `monitoring/`) and wire each one to its own
`test-integration-<suite>` Make target and CI job.

## Conventions

- **Numbered step files** — the `NN-` prefix mirrors the step order in
  `chainsaw-test.yaml`: `10-create-instance.yaml` applies a manifest,
  `10-assert.yaml` asserts the expected outcome of that step.
- **Simulate the operator, except where it matters** — most suites deploy
  with your DB operator disabled (see `deploy-provider-ci` in the Makefile)
  and patch the operator-native CR status (`kubectl patch --subresource
  status`) to simulate readiness. This keeps them fast and deterministic:
  you are testing the *provider's* translation and status logic, not the
  operator itself. Keep at least one suite (`e2e-cluster` here) against the
  real operator (`deploy-provider-e2e`) so a CRD shape the real operator
  actually rejects, or connection details that don't actually connect, don't
  slip through unnoticed.
- **Assert both directions** — after applying an `Instance`, assert the
  operator-native CR spec the provider must produce; after patching the
  operator CR status, assert the `Instance` reaches `phase: Running`.
- **Collect diagnostics on failure** — use the `catch` block in
  `chainsaw-test.yaml` to dump relevant resources and provider logs.

## Running locally

Prerequisites: a running cluster with the OpenEverest CRDs, the OpenEverest
controller, your operator's CRDs, and the provider deployed (see the
`install-crds` and `deploy-provider-ci` Make targets), plus the
[chainsaw CLI](https://kyverno.github.io/chainsaw/latest/quick-start/install/):

```bash
go install github.com/kyverno/chainsaw@latest
```

Then:

```bash
make test-integration             # all suites
make test-integration-core        # just the core suite
make test-integration-e2e-cluster # just the e2e-cluster suite (needs the real operator deployed: deploy-provider-e2e)
```

## Running in CI

`.github/workflows/ci.yaml` runs each suite as a separate job through the
reusable `.github/workflows/integration-test.yaml` workflow, which provisions
a k3d cluster, builds and deploys the provider and the OpenEverest controller,
and invokes the corresponding Make target. To add a suite to CI, add a job to
`ci.yaml` that passes the suite's `make_target`.
