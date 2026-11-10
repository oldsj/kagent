# Runtime Composition

A Session can run its Agent's harness inside a caller-selected development
image instead of the harness image. The caller picks the image; the operator
owns everything that executes the harness.

## Inputs

- **Development image (D)** — `CreateSessionRequest.development_environment`:
  a sha256 digest-pinned `image`, its `platform` (`linux/amd64` or
  `linux/arm64`) and an opaque `policy_identity` the caller uses to name the
  environment policy it resolved the image from. The selection cannot supply a
  command, environment, mounts or capabilities.
- **Runtime payload (R)** — the controller's `runtime_payloads` catalog: one
  digest-pinned image per provider (`claude`, `codex`) and platform, with the
  CLI release it ships. Payload images are built from
  `go/harness/runtime/payload` and contain the static launcher, the harness
  binary, the pinned CLI with its private musl loader, and Git.

## Payload Packaging

The payload mounts at `/opt/mainloop-runtime` and does not depend on D's
libraries: Git, Bash, ripgrep and their helpers run through a private musl
loader, and the CLI binary sits in `libexec/` behind a launcher shim in
`bin/`. `bin/launch` checks the platform selection and the payload manifest
(schema, provider, platform, checksums and CLI version), prepares the durable
runtime directories under `/data` (`workspace`, `adapter`, `generated`,
`claude`, `codex`, `kagent`, `.kagent`, `home`, `cache`, `tmp`) as real 0700
directories, refuses symlinks, then execs the harness with the payload's
environment from `/data`. The harness reads its configuration from the
bootstrap bundle after exec and checks its own CLI version. D must provide a
writable `/tmp` for the Claude harness's temporary files.

## Admission

Selection is refused unless `server.auth.session_policy` allows the Agent and
lists the image's registry authority in `development_environment_registries`.
The check applies to every caller, internal controller work included; an empty
list denies selection.

## Preparation

A composed Session pins a **variant revision** derived from the Agent's latest
successful revision:

1. The controller's composition preparer takes the Agent's current compiled
   target and requires it to be the latest successful revision. The
   WorkerPool must pin `kubernetes.io/arch` to the selected platform, and the
   catalog must have a payload for the harness and platform.
2. `translator.ComposeRevision` derives the variant: image D, entrypoint
   `/opt/mainloop-runtime/bin/launch`, the CLI executable and expected version
   from the payload, and `MAINLOOP_RUNTIME_PLATFORM`. Harness environment that
   would redirect the payload's loader, homes or runtime paths is rejected, as
   is the Claude SDK driver, which the payload does not ship.
3. The ActorTemplate mounts R as a read-only Substrate image volume at
   `/opt/mainloop-runtime` and adds `CHOWN`, `DAC_OVERRIDE`, `FOWNER`,
   `SETGID` and `SETUID` so development-image tooling can drop to its own
   user. No network or mount capability is added.
4. The variant is recorded with `base_revision` and its `composition`, and
   the preparer waits for its golden snapshot. A failed golden removes the
   template so a retry rebuilds it.

Preparation runs outside store transactions. The Session reservation then
verifies, under the Agent definition lock, that the variant's base is still
the latest successful revision and that its composition matches the request.
If the Agent changed meanwhile, the create fails with `ABORTED` and the caller
retries. A retried `request_id` reuses its original preparation.

## Identity and Retention

The composition is part of the revision digest, so variants never collide
with single-image revisions, and single-image digests are unchanged. A
Session reports its `runtime_composition`, read from the pinned revision;
checkpoint forks inherit it the same way. Garbage collection keeps an
unreferenced variant while its base is an active Agent's latest successful
revision, so concurrent Sessions reuse the same golden.
