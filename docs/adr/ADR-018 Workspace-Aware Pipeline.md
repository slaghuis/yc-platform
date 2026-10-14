# ADR-018: Workspace-Aware Pipeline

**Status:** Accepted
**Date:** 2026-10-14
**Context:** yc-platform is a Go workspace: a `go.work` at the repo root lists 31 modules (10 libs, 17 services, 4 tools) and there is no root `go.mod`. Services resolve the shared libs through `go.work`, not through `replace` directives. The agent-driven pipeline (`pipeline-lib`, exposed to agents through `pipeline-mcp`) assumes a single module at the project root: it reads `<root>/go.mod` for the Go version, runs `go vet ./...`, `go test ./...`, `golangci-lint run ./...` and `govulncheck ./...` from the root, and builds one `main_package` into one image. At a workspace root those commands fail with `directory prefix . does not contain modules listed in go.work`. Rooting a pipeline at a single service directory works only until that service imports a lib, because the lib then sits outside the mounted source. The workspace is delivered as one task list (see `docs/implementation-plan.md`), so it needs one pipeline, not one per module.

## Decision

**Extend `pipeline-lib` with a workspace mode, and run yc-platform through a single pipeline.** One `pipeline.yaml` at the repo root, one `bin/pipeline` binary, one `yc-platform` entry in the pipeline-mcp registry. Each run lints, tests and scans every module in `go.work`, then builds one image per deployable service.

Single-module projects (for example data-loader) keep their current behaviour. The change ships as `pipeline-lib` v0.2.0.

## Rules

1. **Detection.** Workspace mode is on when `<root>/go.work` exists and `<root>/go.mod` does not. `workspace: true | false` in `pipeline.yaml` overrides detection.
2. **Module set.** The modules are exactly the `use` entries of `go.work` (read with `go work edit -json`). A module outside `go.work` is outside the pipeline; add it to `go.work` to include it. Today that excludes `pipeline/` (on purpose) and `test/e2e` (to be decided separately).
3. **Go version.** The highest `go` directive across `go.work` and all member `go.mod` files, raised further by `go_version` in `pipeline.yaml` if that is higher.
4. **Source.** The whole repo root is mounted, with the existing exclusions (`.git`, `bin`, `dist`, …). Dependencies are fetched once with `go mod download` at the root, which works in workspace mode.
5. **Go toolchain stages.** `go vet` and `go test` run once from the root over every module's package pattern (`<module path>/...` for each module), so they share one build cache. Coverage is one merged profile, and `coverage_min` applies to that total.
6. **External tools.** `golangci-lint` and `govulncheck` run from each module directory in turn, with `go.work` active. A failure in one module does not skip the rest; the stage fails if any module fails.
7. **Build targets.** `main_package` is not used in workspace mode. Targets are listed under `build.targets`, or discovered by the default glob `services/*/cmd/*`. Each target's name is its directory name (for example `yc-identity`). A service without a `cmd/<name>` directory is skipped, as `task build` does today.
8. **Image naming.** Each target becomes the image `<build.registry>/<target>:<version>`, matching the `ghcr.io/slaghuis/yc-platform/<service>` naming in `.github/workflows/release.yml`. With no registry, each image is exported to `dist/<target>.tar`.
9. **Report.** One JSON report per run, with the existing stage names (`lint`, `test`, `scan`, `build`, `deploy`). Results for each module and each target go under the stage's `metrics`, so an agent can see which module or service failed without the report schema changing for single-module users.
10. **Deploy.** The deploy stage receives every image reference built in the run and applies `deploy.manifests` once for the chosen environment. How manifests consume per-service image references is out of scope here (see Follow-ups).
11. **The pipeline module stays outside the workspace.** `pipeline/` is built with `GOWORK=off` into `bin/pipeline`, which is git-ignored.

## Example configuration

```yaml
service: yc-platform
workspace: true            # optional; detected from go.work

build:
  registry: ${REGISTRY}    # e.g. ghcr.io/slaghuis/yc-platform
  targets:                 # optional; default discovers services/*/cmd/*
    - name: yc-identity
      main_package: ./services/yc-identity/cmd/yc-identity
```

## Consequences

**Positive**
- One command (`pipeline_run service=yc-platform command=full`) verifies the whole workspace, which matches how the work is planned and reviewed.
- Libs and services are always tested together at the same commit, so a lib change that breaks a service fails the run.
- One registry entry and one Telegram approval cover a release, not seventeen.
- Single-module projects are unaffected.

**Negative**
- Every run covers every module. Dagger's content-addressed caching keeps unchanged modules cheap, but a cold run is slower than running one service.
- `pipeline-lib` gains a second mode to maintain and test.
- A release deploys all services together. Releasing one service on its own needs a later extension (for example a `-target` flag).
- `.github/workflows/` and `deploy/docker/Dockerfile.service` remain a second definition of lint, test and image build, and can drift from the pipeline until they are reconciled.

## Alternatives Considered

- **One pipeline per service** (a `pipeline.yaml` and registry entry per service, rooted at `services/<name>`): Rejected. It breaks once services import libs, since `go.work` and `libs/` are outside the mounted source, and it splits one task list into seventeen pipelines and approvals.
- **A single root `go.mod`** (turn the workspace into one module): Rejected. It gives up per-module dependency boundaries and versioning, which the monorepo layout (`docs/onboarding/monorepo-layout-and-tooling.md`) is built on.
- **Workarounds in `pipeline/main.go` using `cli.RegisterHook`**: Rejected. Hooks can only transform the source directory; they cannot change the commands the stages run.
- **Drop `pipeline-lib` and use Taskfile or GitHub Actions only**: Rejected. That loses agent-callable stages through pipeline-mcp, the structured JSON report, and the Telegram approval gate.
- **Run only the modules affected by a change**: Deferred. It is worth adding once full runs are measurably slow; caching makes it unnecessary for now.

## Follow-ups

- Decide how Kubernetes manifests in `deploy/k8s` receive per-service image references (kustomize image overrides, Helm values or templating). `deploy/k8s` does not exist yet, so `release` cannot succeed until this is decided.
- Decide whether `.github/workflows/ci.yml` and `release.yml` should call `bin/pipeline` or be retired, so there is one definition of the pipeline.
- Decide whether `test/e2e` joins `go.work`, and whether e2e runs as the pipeline's `integ` stage.
