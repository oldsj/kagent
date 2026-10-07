# Development environments and runtime payloads

Claude and Codex Sessions can optionally select a trusted development image independently of the harness runtime. `CreateSessionRequest.development_environment` contains a digest-pinned `image`, `platform` (`linux/amd64` or `linux/arm64`), and immutable `policy_identity`. Omitting it retains the single-image path and its revision digest.

The operator sets `KAGENT_RUNTIME_PAYLOAD_CATALOG` to a JSON object keyed by provider/platform. For example:

```json
{
  "claude/linux/arm64": {
    "image": "registry.example/runtime-claude@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "cliVersion": "2.1.260"
  }
}
```

Callers cannot supply the payload image or CLI version. Each composed revision binds the development image, payload image, provider, platform, payload schema, CLI version, policy identity, configuration and environment. The ActorTemplate runs the development image and mounts the payload through Substrate's read-only native image volume at `/opt/mainloop-runtime`. Its command is `/opt/mainloop-runtime/bin/launch`. Reserved runtime paths and owned environment variables cannot be overridden by harness configuration.

The template sets reserved `MAINLOOP_RUNTIME_PLATFORM` from the trusted selection. Before accessing durable state or running a harness, launch requires that selection to match both the executing binary's OS/architecture and the payload manifest. This also applies when D/R digests identify multi-architecture indexes: resolving the host's image child cannot substitute a different platform for the Session selection.

Preparation requires the referenced WorkerPool's `spec.template.nodeSelector["kubernetes.io/arch"]` to match the selected architecture. A missing architecture constraint, unknown pool or mismatched architecture is rejected before Substrate preparation. Workers run on Linux; an explicit `kubernetes.io/os` selector must agree. Configure the production arm64 pool through GitOps with `kubernetes.io/arch: arm64` (and optionally `kubernetes.io/os: linux`). Mixed-architecture pools without an explicit architecture constraint cannot prepare compositions. Launch remains the execution-time check if pool configuration subsequently drifts.

Preparation creates an immutable template and records its revision before reserving the Session, without holding a database transaction across Substrate calls. Pending or failed goldens remain recorded for retry and diagnostics. A composed golden can succeed independently of the legacy image. Reservation locks the active definition and selected revision, then checks that the prepared composition still belongs to the active Agent definition. It never changes the shared Agent, Harness or latest successful legacy revision.

Request-ID retries retain the originally selected payload even after catalog updates. Checkpoint forks retain the source Session's exact composition. Garbage collection retains composition variants while their base definition is active; Sessions and checkpoints retain their selected revision afterward. This conservative policy also protects the interval between preparation and reservation. A different image pair creates a new revision; a Full snapshot never receives a replacement pair.

## Payload packaging

`go/harness/runtime/payload/runtime-lock.json` is the shared CLI version and per-architecture artifact checksum source for both legacy Dockerfiles and payload builds. The payload includes static Go launch/harness binaries, untouched vendor CLI bytes, private musl libraries, Bash, Git and ripgrep helpers, and a checksummed manifest. Dynamic vendor CLIs run through the bundled musl loader; rewriting the Claude executable's ELF layout breaks its embedded Bun payload. Other dynamic helpers use the private payload interpreter and library path.

The launcher checks manifest metadata, platform and file hashes, verifies writable durable state, establishes native homes beneath `/data`, enters `/data/workspace`, and executes the harness. Harness configuration names the CLI by absolute payload path and pins its expected version. `launch --check` verifies packaging and CLI version without contacting a model. Payload files remain world-readable/executable. Stock Substrate currently runs actors as root; Claude retains `IS_SANDBOX=1`.

Build locally from `go/harness/runtime/payload` with `make build PROVIDER=claude PLATFORM=linux/amd64 IMAGE=runtime-claude:local` (or `PROVIDER=codex`). Publishing and catalog deployment are separate operator steps.

## Trust boundary

Environment selection defaults to denied, independently of ordinary Session authorization. Only service-token mode installs the explicit selection policy and preparer. Insecure and trusted-proxy callers cannot select D, even when their user identity is `mainloop`. The policy requires the verified Mainloop service principal, an allowed Agent, a digest-pinned D from an exact registry authority allowlist, a platform present in the operator payload catalog, and a nonblank policy identity. Internal controller sessions do not bypass this selection boundary.

Configure registry authorities with `developmentEnvironmentRegistries` in `KAGENT_AUTH_SERVICE_POLICY`, or Helm `controller.auth.serviceToken.developmentEnvironmentRegistries`. Entries are exact hosts with optional ports, without schemes or repository paths; an empty list denies selection. The Session service also requests `create` authorization on a `DevelopmentEnvironment` resource named by the selected digest reference. Development-image registration must separately validate ABI requirements, image-declared user, immutable policy identity and grants; those checks remain the trusted Mainloop caller's responsibility.

Local amd64 glibc packaging checks do not prove arm64 execution, live gVisor provisioning, Full snapshot restoration or registry retention. Those require integration and release evidence.
