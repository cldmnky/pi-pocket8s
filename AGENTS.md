# AGENTS.md

Instructions for coding agents working in this repository. `README.md` is for
humans; this file is the short version of what an agent must know to change
this project without breaking it. Read `README.md`, `docs/research.md`,
`portal/README.md` and `images/README.md` before non-trivial changes — they
record the security decisions this repo is built on.

pi-pocket8s packages [pi-pocket](https://github.com/TannerMidd/pi-pocket) for
OpenShift: a UBI 10 image with a developer toolchain, a Go configuration portal,
and one Helm chart. The workspace pod has namespace `admin` rights and anyone
who can steer the agent can run code in it. Treat every change as security
relevant.

## Commands

Run these before you call any change finished.

```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements-dev.txt   # once
make test PYTHON=.venv/bin/python   # chart tests + go test -race + node --test
make lint                           # helm lint + go vet
make format                         # gofmt -w on portal/**.go
make dry-run                        # helm template | kubectl apply --dry-run=server (needs a live namespace)
```

CI (`.github/workflows/validate.yml`) runs `make lint test` and then
`test -z "$(gofmt -l $(find portal -name '*.go'))"`, so run `make format`
before committing.

Focused runs:

```bash
.venv/bin/python tests/test_chart.py                                  # one file
cd portal && go test -race ./internal/server/ -run TestName           # one test
node --test extensions/web-search.test.mjs                           # one node test
helm template pi-pocket charts/pi-pocket -n test-pocket -f deploy/openshift-values.yaml
```

Two things that surprise people:

- `helm template` output must render under **Helm 3 (CI pins v3.19.0) and Helm 4
  (the image ships v4.3.0)**. A local `helm` is not proof; check both if you
  touch chart logic or templating functions.
- `TestInClusterClientRequiresEnvironment` fails when the test suite runs
  **inside a pod that has service-account files mounted** (it asserts their
  absence). That is an environment artefact, not a regression — verify with
  `ls /var/run/secrets/kubernetes.io/serviceaccount/` before "fixing" it.

Image builds are not part of `make test` and need network plus `podman`:

```bash
make image          # pi-pocket image (context is the repo root, not images/)
make portal-image   # portal image (context is portal/)
```

## Project map

| Path | What lives there |
| --- | --- |
| `charts/pi-pocket/` | The Helm chart: deployments, PVC, RBAC, ingresses, NetworkPolicy, Secret handling, `validate.yaml` fail-fast guards |
| `portal/` | Go module (`github.com/cldmnky/pi-pocket8s/portal`): HTTP server, embedded vanilla-JS SPA in `web/`, kube client, GitHub App/OAuth code. Tests sit next to the code as `*_test.go` |
| `images/` | Containerfile for the workspace image, `entrypoint.sh`, `embed-patch.sh`, and Node ESM helpers with `*.test.mjs` tests |
| `extensions/` | `web-search.ts` plus `web-search/`: the agent's `web_search` tool, installed by the Containerfile as a Pi Pocket built-in extension. The provider code under `web-search/vendor/` is vendored third-party code (MIT, see its `NOTICE.md`): copy a new release rather than editing it |
| `tests/test_chart.py` | Renders the real chart and asserts security/lifecycle contracts |
| `deploy/openshift-values.yaml` | The example cluster profile used by tests, lint and `make install` |
| `docs/research.md` | Sources and live-cluster evidence behind the design choices |

## Stack

- Go 1.24 (`portal/go.mod`); dependencies are deliberately minimal (only
  `golang.org/x/crypto`, plus indirect `x/sys`). The SPA has no build step, no
  CDN and no bundler — plain JS, CSS and vendored `qrcodegen.js`.
- Helm chart, plus Python 3.13 + `PyYAML==6.0.3` for chart assertions
  (`requirements-dev.txt`, pinned).
- Node 24 ESM for the `images/*.mjs` helpers and their `node --test` suites.
- Images: `registry.access.redhat.com/ubi10/ubi` for the workspace,
  `ubi10/ubi-minimal` for the portal, both UID/GID 1000. Upstream pi-pocket is
  **pinned by commit** (`PI_POCKET_COMMIT` in `images/Containerfile`); scheduled
  builds only refresh the base image and RPMs.

## Conventions

- **Go**: `gofmt`/`go vet` clean, table-driven tests, no framework. Return
  errors matching the existing `errors.go` style; fail closed on auth paths.
- **YAML/Helm**: 2-space indent. New values get a comment in `values.yaml`
  explaining the consequence, not just the type. Unsafe combinations belong in
  `templates/validate.yaml` as a `fail`, not in prose.
- **Shell**: `set -euo pipefail`, and prefer explicit failure messages over
  silent fallbacks.
- **Docs**: match the existing register — dense, factual, state *why* a
  restriction exists, and never claim something that was not verified. Update
  `README.md`/`docs/research.md` in the same change when behaviour changes.
- **Vendored code**: keep `extensions/web-search/vendor/` byte-identical to the
  upstream release it names; adapter changes belong next to it, never inside it.
- **Commits**: imperative subject, prefixed with the area when it is one area —
  `Portal: …`, `Image: …`, `CI: …`, `Docs: …`. Work happens on a
  `feat/<topic>` branch and lands on `main` through a pull request.

## Boundaries

Always:

- Run `make lint test` before finishing, and add or update tests for what you
  changed.
- Keep credentials out of Git and out of Helm values; they live in Kubernetes
  Secrets (`runtimeSecret.existingSecret`, `portal.tokenSecret`).
- Keep `make dry-run` optional and honest: it needs a live namespace, so it is
  not a substitute for the offline tests.

Ask first:

- Adding any Go dependency, bumping `PI_POCKET_COMMIT`/`PI_POCKET_VERSION`, or
  changing pinned tool versions (kubectl, helm, oc, gh, Chromium, ripgrep).
- Any change to the security posture: SCC choice, seccomp profile,
  `hostUsers`, capabilities, RBAC scope, NetworkPolicy.
- Anything that pushes to `quay.io/cldmnky/*`, touches a live cluster, or
  rewrites generated/retained Secrets.

Never:

- Commit secrets, tokens, `kubeconfig`, `.env*` or private keys.
- Weaken, skip or delete a test to make CI pass — `tests/test_chart.py` encodes
  contracts on purpose (`hostUsers: false`, `Unconfined` seccomp for
  nested-container, single-writer SQLite, retained PVC, RBAC scope). Fix the
  chart instead.
- Set `replicas` above 1, or introduce a second process writing the SQLite
  database.
- Select NFS-backed storage for the workspace PVC, modify the cluster's
  built-in SCCs, or assume `cluster-admin` exists.
- Apply an offline `helm template` render over a live installation: `lookup` is
  how existing credentials survive an upgrade, and an offline render emits
  fresh defaults.
- Edit `.github/workflows/**` to publish images or push from pull requests.

## Nested files

Pi and Pi Pocket load this file (or `CLAUDE.md`, or an `AGENTS.override.md`) for
any session in this directory or below it, and merge it with files in parent
directories — a nested `AGENTS.md` applies only to sessions run inside its own
subtree, and the closest file's rule wins. Add one in `portal/` or `images/` only
when that subtree genuinely needs rules the root file should not carry.
