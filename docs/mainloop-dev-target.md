# Mainloop dev_sync staging target: kv

This fork's `kv` vcluster can act as a Mainloop `dev_sync` staging target for
the kagent controller. The production controller image
(`ghcr.io/oldsj/kagent-controller`, built from `go/Dockerfile`) is a
stripped Alpine image with no Go toolchain, so synced source cannot be
rebuilt inside it. `kagent-controller-dev` (built from `go/Dockerfile.dev`,
published by `.github/workflows/mainloop-fork-images.yaml` alongside the
other fork images) exists for this purpose: it ships a Go toolchain, the
repository's `go/` source baked in at `/src/go`, warmed module/build caches,
and an entrypoint (`go/scripts/controller-dev-entrypoint.sh`) that builds and
runs the controller and rebuilds and restarts it whenever `dev_sync` copies
in changed source.

## Staging-target settings

| Setting | Value |
| --- | --- |
| `kind` | `vcluster` |
| `namespace` | `kagent-staging` (host namespace running the `kv` vcluster) |
| `vcluster_name` | `kv` |
| `vcluster_namespace` | `kagent-dev` (namespace inside `kv` where the `kagent` chart is installed) |
| workload label selector | `app.kubernetes.io/name=kagent,app.kubernetes.io/instance=<release-name>,app.kubernetes.io/component=controller` (matches the `<release-name>-controller` Deployment; substitute the Helm release name used for the `kv` install, `kagent` unless installed otherwise) |
| command container | `controller` (the only container in that pod) |

## sync_paths

```
prefix: go
dest:   /src/go
excludes:
  - .git/**
  - sandbox/**        # builds the separate kagent-sandbox-guest image, not the controller
  - "**/*_test.go"     # never compiled into `go build`, just extra bytes to sync
```

`go.mod`/`go.sum` are included (not excluded), so a sync that adds a
dependency works too — but note that requires the controller pod to reach
`proxy.golang.org` (or a configured `GOPROXY`) from inside `kv`; the image
sets `GOTOOLCHAIN=local` so it never tries to download a different Go
*toolchain* version, only new module dependencies need network access.
Purely local edits never need network access: `GOCACHE`/`GOMODCACHE` are
warmed into the image at build time, so an in-place rebuild after a sync is
an incremental `go build`, not a cold one.

## Restart, health, and smoke commands

The entrypoint already polls `/src/go` for changed `*.go` files every 2s
(`DEV_POLL_INTERVAL`) and rebuilds/restarts on its own, so an explicit
restart command is optional. To force an immediate check instead of waiting
for the next poll:

```
restart: kill -HUP 1
```

(the entrypoint script runs as PID 1 in the container.)

The controller's only unauthenticated HTTP surface is the same `/health`
path the chart's own startup/readiness probes use, served on the same
multiplexed gRPC/HTTP port (`controller.service.ports.targetPort`, default
`8083`) real clients use. The image bakes in a tiny dependency-free helper,
`kagent-healthcheck` (`go/scripts/healthcheck/main.go`), instead of relying
on `curl`/`wget` being present in the base Go toolchain image:

```
health: kagent-healthcheck
smoke:  kagent-healthcheck http://127.0.0.1:8083/health
```

Both run the same check; `smoke` is kept distinct in config only because
`dev_sync` treats them as separate hooks — there is currently only one
unauthenticated surface worth checking from inside the pod.

## Chart values infra must set for `kv`

```yaml
controller:
  image:
    repository: oldsj/kagent-controller-dev
    tag: "0.0.0-<7-character-source-sha>"   # from the image's publish step summary
  securityContext:
    readOnlyRootFilesystem: false   # the entrypoint rebuilds into /src/go and $GOCACHE at runtime
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: "2"
      memory: 2Gi
```

`registry` defaults to `ghcr.io` already (top-level `registry` value), so no
registry override is needed, only `repository`/`tag`.

`readOnlyRootFilesystem: false` is the only required deviation from the
chart's production defaults: the dev image still runs as the same non-root
UID (`65532`) as production, with `runAsNonRoot: true`, and needs no extra
capabilities, volumes, or privilege escalation — it only needs a writable
root filesystem so `go build` can write the rebuilt binary and populate the
build cache under `/src/go` and `/home/nonroot`, both already owned by
`65532:65532` in the image.

The `resources` bump is because `go build` for this module (istio, AWS/Azure
SDKs, k8s client-go, OTel, etc. are all transitive dependencies) needs
noticeably more memory than running the already-linked production binary;
the chart's production defaults (`128Mi`/`512Mi`) are sized for the latter.

Everything else — database connection, auth mode, watched namespaces, OTel
config, and so on — comes from the same `controller.*` and `database.*`
chart values as a production install; the dev image does not add or change
any controller flags or environment variables. The built binary is started
with no arguments, exactly like the production image's `ENTRYPOINT
["/app"]`: the controller takes all of its configuration from the
environment the chart already injects (`envFrom`/`env` on the container),
never from CLI flags, so there is nothing else for the dev image to
replicate.
