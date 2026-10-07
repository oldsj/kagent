# Development

The [environment variable reference](docs/env.md) is generated from
`go/core/pkg/env`. Register user-configurable settings there, including settings
consumed by Python, the UI, or standalone runtimes. Exclude controller-generated
payloads, credentials, private paths, and other internal process wiring.
Keep defaults and descriptions aligned with
their readers, then run `make env-docs`. Use `make env-docs-check` to run the same
freshness check as CI.

Register shared settings once, passing each consuming component to
`RegisterStringVar`, `RegisterBoolVar`, `RegisterIntVar`, or
`RegisterDurationVar`, for example:

```go
RegisterStringVar("KAGENT_LOG_LEVEL", "info", "Logging level.", ComponentController, ComponentCLI, ComponentAgentRuntime)
```

The reference lists shared settings under each component. `kagent env --component`
matches any registered component; JSON output includes a `components` array.

Kagent-owned settings use the `KAGENT_` prefix. Keep names defined by upstream
SDKs and tools, such as `OTEL_*`, provider credentials, and `KUBECONFIG`, unchanged.
The pre-release rename replaces the old unprefixed names: for example,
`LOG_LEVEL` becomes `KAGENT_LOG_LEVEL`, `HTTP_BIND_ADDRESS` becomes
`KAGENT_HTTP_BIND_ADDRESS`, and the Go ADK's `PORT` becomes `KAGENT_PORT`.
The old names are no longer read by kagent.

Both ADKs use `KAGENT_PORT` for their A2A listener; Python's former
`KAGENT_A2A_GRPC_ADDRESS` is removed. The controller sets `80`. Standalone
defaults remain `8080` for Go's shared HTTP/gRPC listener and `80` for Python's
gRPC listener; Python's HTTP port is configured separately with `--port`.

UI containers and Vite development use the same `KAGENT_*` inputs, documented in
`ui/.env.example`. UI build-time switches use `KAGENT_UI_VITE_*`; extension settings
use `KAGENT_UI_EXTENSION_*`. Other `KAGENT_*` settings are not exposed to the browser.

To understand how to develop for kagent, it's important to understand the architecture of the project. Please refer to the [README.md](README.md#architecture) file for an overview of the project.

When making changes to `kagent`, the most important thing is to figure out which piece of the project is affected by the change, and then make the change in the appropriate folder. Each piece of the project has its own README with more information about how to setup the development environment and run that piece of the project.

- [python](python): Contains the code for the ADK engine.
- [go](go): Contains the code for the kubernetes controller, and the CLI.
- [ui](ui): Contains the code for the web UI.

## Mainloop fork fixture gate

In `oldsj/kagent`, `.github/workflows/mainloop-fork-ci.yaml` runs the
`chatgptrefresh` package's fixture tests (`-count=1 -timeout=180s`) and `go vet`
on a standard GitHub-hosted runner. It runs for pull requests targeting
`mainloop-substrate-0-4`, pushes to that branch, and manual dispatches, without
path filters. The upstream CI workflow is unchanged.

PR runs test GitHub's merge candidate; push and manual runs test the event's
commit. The run summary records the ref, event SHA, checkout SHA, and PR head
SHA so publication checks can be matched to the intended source. After
publication, when the workflow is available for dispatch on the default branch,
select the source branch in **Run workflow** or use:

```shell
gh workflow run mainloop-fork-ci.yaml --repo oldsj/kagent --ref SOURCE_BRANCH
```

Verify the recorded SHA against the published candidate before accepting a
remote result. Bootstrap fixtures participate once their changes are in the
tested checkout. This gate uses synthetic credentials, fake Kubernetes clients,
and a fixture CLI; it needs no provider secrets or subscription. Fork image
publication uses the separate workflow below; live provider calls and
container/Kubernetes E2E suites remain opt-in validation. A passing fixture
gate does not prove image CI, live credential refresh, or deployment readiness.

## Mainloop fork images

In `oldsj/kagent`, `.github/workflows/mainloop-fork-images.yaml` publishes
`kagent-controller`, `kagent-claude-harness`, and `kagent-codex-harness` to
`ghcr.io/oldsj` on pushes to `mainloop-substrate-0-4` and manual dispatches.
It uses the native `ubuntu-24.04-arm` runner and builds only `linux/arm64`,
with tags `0.0.0-<7-character-source-SHA>`. It does not run on pull requests.
OCI labels record the source repository and full commit SHA; each job summary
records its platform, tag, and immutable `name@digest` for deployment pins.
Update pins through a separately reviewed infrastructure PR. This workflow
builds neither Helm charts nor the infrastructure-owned workspace harness.

The package owner must grant `oldsj/kagent` **write** access under **Manage
Actions access** in each existing GHCR package's settings:
`kagent-controller`, `kagent-claude-harness`, and `kagent-codex-harness`.
Publication uses the job's `GITHUB_TOKEN` with `packages: write` and
`contents: read`; no separate registry secret is needed.

## Nightly releases

The [Nightly Release workflow](https://github.com/kagent-dev/kagent/actions/workflows/nightly.yaml)
runs at 02:00 UTC on the default branch, skipping builds when its commit matches
the previous successful nightly. Maintainers can also run it manually on the
default branch to force a rebuild.

Use **Run workflow** on that page, or run
`gh workflow run nightly.yaml --ref main`. Both nightly and tagged releases use
the shared `publish-image`, `publish-helm`, and `build-release-artifacts` composite
actions in `.github/actions`.

Each build publishes all six component images and the `kagent` and `kagent-crds`
Helm charts as `0.0.0-alpha.g<12-character-commit>`. These are development builds;
nightly runs do not publish Python packages to PyPI or create GitHub releases.

To install a nightly, use the version from its workflow summary for both charts
with your usual installation values. Replace the example version below:

```shell
NIGHTLY_VERSION=0.0.0-alpha.g0123456789ab
helm upgrade --install kagent-crds oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
  --version "$NIGHTLY_VERSION" --namespace kagent --create-namespace
helm upgrade --install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
  --version "$NIGHTLY_VERSION" --namespace kagent -f your-values.yaml
```

Each workflow run includes a changelog in its summary and an artifact containing
CLI binaries, checksums, and commit-specific chart archives.

## Dependencies

Before you can run kagent in Kubernetes, you need to have the following tools installed:

### Required Dependencies

- **Kind** (v0.27.0+)
- **kubectl** (v1.33.4+)
- **Helm**
- **Go** (v1.27.0+)
- **Docker**
- **Docker Buildx** (v0.23.0+)
- **Make**

### Installation Verification

You can verify your installation by running:

```shell
# Check core dependencies
kind version
kubectl version
helm version
go version
docker version
docker buildx version
make --version
```

## How to run everything in Kubernetes

1. Create a cluster:

```shell
make create-kind-cluster
```

1. Configure the cluster and set the default namespace `kagent`:

```shell
make use-kind-cluster
```

1. Set your model provider:

```shell
export KAGENT_DEFAULT_MODEL_PROVIDER=openAI
#or
export KAGENT_DEFAULT_MODEL_PROVIDER=anthropic
```

1. Set your providers API_KEY:

```shell
export OPENAI_API_KEY=your-openai-api-key
#or
export ANTHROPIC_API_KEY=your-anthropic-api-key
```

Alternatively, create a `.env` file at the repo root (gitignored). This file is
loaded by the Makefile (via `-include .env`) to inject environment variables
when you run `make` targets:

```shell
# Set your provider (supported: openAI, anthropic, azureOpenAI, gemini, ollama)
KAGENT_DEFAULT_MODEL_PROVIDER=openAI

# Set the corresponding API key for your provider
OPENAI_API_KEY=your-openai-api-key
# ANTHROPIC_API_KEY=your-anthropic-api-key
# GOOGLE_API_KEY=your-google-api-key
```

1. Build images, load them into kind cluster and deploy everything using Helm:

```shell
make helm-install
```

To apply personal Helm overrides without committing them, create
`helm/kagent/values.local.yaml` (gitignored) and set `KAGENT_HELM_EXTRA_ARGS` in
your `.env` file (which is read by the Makefile when you run `make`):

```shell
# .env
KAGENT_HELM_EXTRA_ARGS=-f helm/kagent/values.local.yaml
```

To access the UI, port-forward to the UI port on the `kagent-ui` service:

```shell
kubectl port-forward svc/kagent-ui 8001:8080
```

Then open your browser and go to `http://localhost:8001`.

### Addons

Optional addons are available to enhance your development environment with
observability and infrastructure components.

**Prerequisites:** Complete steps 1-3 above (cluster creation and environment variables).

To install all addons:

```shell
make kagent-addon-install
```

This installs the following components into your cluster:

| Addon          | Description                          | Namespace      |
|----------------|--------------------------------------|----------------|
| Istio          | Service mesh (demo profile)          | `istio-system` |
| Grafana        | Dashboards and visualization         | `kagent`       |
| Prometheus     | Metrics collection                   | `kagent`       |
| Metrics Server | Kubernetes resource metrics          | `kube-system`  |

PostgreSQL is deployed automatically as part of `make helm-install` via the bundled Helm chart. The optional addons above provide observability components.

> **pgvector:** The default bundled PostgreSQL image (`postgres:18.3-alpine`) does not include the pgvector extension. If you need vector features (e.g. long-term memory), either use an external PostgreSQL instance with pgvector installed, or override the bundled image to `pgvector/pgvector:pg18-trixie` and set `database.postgres.vectorEnabled=true`. The `make helm-install` target does this automatically for local development.

Verify the database connection by checking the controller logs:

```shell
kubectl logs -n kagent deployment/kagent-controller | grep -i postgres
```

### Troubleshooting

### buildx localhost access

The `make helm-install` command might time out with an error similar to the following:

> ERROR: failed to solve: DeadlineExceeded: failed to push localhost:5001/kagent-dev/kagent/controller

As part of the build process, the `buildx` container tries to build and push the kagent images to the local Docker registry. The `buildx` command requires access to your host machine's Docker daemon.

Recreate the buildx builder with host networking, such as with the following example commands. Update the version and platform accordingly.

```shell
docker buildx rm kagent-builder-v0.23.0

docker buildx create --name kagent-builder-v0.23.0 --platform linux/amd64,linux/arm64 --driver docker-container --use --driver-opt network=host
```

Then run the `make helm-install` command again.

### Run kagent and an agent locally

create a minimal cluster with kind. scale kagent to 0 replicas, as we will run it locally.

```bash
make create-kind-cluster helm-install-provider helm-tools push-test-agent push-test-skill
kubectl scale -n kagent deployment kagent-controller --replicas 0
```

Run kagent with `KAGENT_A2A_DEBUG_ADDR=localhost:8080` environment variable set, and when it connect to agents it will go to "localhost:8080" instead of the Kubernetes service.

Run the agent locally as well, with `--net=host` option, so it can connect to the kagent service on localhost. For example:

```bash
docker run --rm \
  -e KAGENT_API_URL=http://localhost:8083 \
  -e KAGENT_GATEWAY_URL=http://localhost:8083 \
  -e KAGENT_NAME=kebab-agent \
  -e KAGENT_NAMESPACE=kagent \
  --net=host \
  localhost:5001/kebab:latest
```

## Telemetry

The telemetry contract is a Weaver registry in `telemetry/registry`. The Go
constants in `go/pkg/telemetry/conv`, the Python constants in
`kagent.core.telemetry.conv`, and `docs/architecture/telemetry-contract.md` are
generated from it. Never edit them by hand. After a registry change, run:

```bash
make semconv-generate   # regenerate everything from the registry
make semconv-verify     # what CI runs: check, policy tests, generate, drift check
```

These targets need either Docker or a local `weaver` of exactly the version in
`telemetry/versions.env`. A different local version is refused, so a green run
on a laptop means the same as in CI. See
[docs/architecture/telemetry.md](docs/architecture/telemetry.md) for the contract.

To look at traces locally, `make otel-local` starts Jaeger with an OTLP receiver
on ports 4317 and 4318 and its UI on http://localhost:16686. Point an install
at it with `--set otel.traces.enabled=true --set otel.exporter.otlp.endpoint=http://<host>:4317`.
