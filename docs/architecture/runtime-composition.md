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

Mainloop code Sessions with an explicit development environment and Git workspace also require [native workspace preparation](native-workspace-preparation.md) before task admission. `PrepareSessionWorkspace` binds the original Create request, D/R selection, prepared revision, active runtime generation and Actor UID. The runtime checks out the exact source and installs a separately digested fixed role overlay without starting a native turn. This action does not select a replacement image or change the compiled revision. Standalone Sessions and Git-free coordinators retain their existing task behavior.

## Payload packaging

`go/harness/runtime/payload/runtime-lock.json` is the shared CLI version and per-architecture artifact checksum source for both legacy Dockerfiles and payload builds. The payload includes static Go launch/harness binaries, untouched vendor CLI bytes, private musl libraries, Bash, Git and ripgrep helpers, and a checksummed manifest. Dynamic vendor CLIs run through the bundled musl loader; rewriting the Claude executable's ELF layout breaks its embedded Bun payload. Other dynamic helpers use the private payload interpreter and library path.

The launcher checks manifest metadata, platform and file hashes, verifies writable durable state, establishes native homes beneath `/data`, enters `/data/workspace`, and executes the harness. Harness configuration names the CLI by absolute payload path and pins its expected version. `launch --check` verifies packaging and CLI version without contacting a model. Payload files remain world-readable/executable. Stock Substrate currently runs actors as root; Claude retains `IS_SANDBOX=1`. Immediately before executing the harness, launch also prepares the filesystem for development-image tooling; see [Process identity and capabilities](#process-identity-and-capabilities).

## Process identity and capabilities

Substrate always runs the actor process as uid 0 and ignores the image's OCI `User`. Its default capability set is `AUDIT_WRITE`, `KILL` and `NET_BIND_SERVICE`, and gVisor has no ambient capabilities. Development images, however, ship tooling that expects to drop to the image user. For example, a test PostgreSQL wrapper uses `runuser` to start PostgreSQL as the image user (PostgreSQL refuses euid 0), chowns its scratch directories, and removes them as root afterwards.

Composed ActorTemplates therefore add exactly `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETGID` and `SETUID` to the default set, and drop nothing. Legacy single-image templates and standalone sandboxes are unchanged. The profile is fixed in the translator (`Composition.ContainerCapabilities`), so neither Session callers nor the payload catalog can choose it. The revision digest includes it, so changing the profile creates new revisions, ActorTemplates and goldens rather than conflicting with existing immutable templates.

The agent already ran as uid 0 and owned every file in the sandbox, so these capabilities grant no new access to credentials or provider state. They add DAC bypass for files owned by another user, ownership changes, and uid switching. No network or mount capability is granted: egress is enforced outside the sandbox, credentials are injected at the gateway, and the payload, identity and trust-bundle mounts stay read-only. gVisor remains the isolation boundary. The agent still runs as root; running the harness itself as the image user is not supported on stock Substrate, because image-layer files appear root-owned inside the actor.

On a cold launch (golden creation, a Data-scope resume or a crash restart), launch prepares the filesystem as root after all validation and before exec. Every step is best-effort and logs a `runtime launch: warning:` line to stderr instead of failing:

- `/` gains at least `0755`. Substrate composes the actor root with mode `0700` ([agent-substrate/substrate#2035](https://github.com/agent-substrate/substrate/issues/2035)), so a non-root process could not resolve any path. The step is a no-op once the root is already traversable.
- `/data` is set to `0711`: other users can traverse to scratch directories but cannot list it.
- The runtime directories `workspace`, `adapter`, `generated`, `home`, `claude`, `codex`, `cache` and `tmp` are reset to `0700` and owned by root. This is reasserted on every cold launch, because the agent can now loosen them. Only the directories themselves are reset; their contents are not walked.
- Any other top-level entry under `/data` owned by a non-root uid or gid is chowned recursively to root, without following symlinks. Substrate's atelet holds no capabilities and cannot delete a DurableDir containing non-root-owned directories ([agent-substrate/substrate#2034](https://github.com/agent-substrate/substrate/issues/2034)). Scratch left behind when tooling dies mid-run would otherwise block cleanup.

**Contract:** image-user-owned state at the top level of `/data` does not survive a cold start; it is returned to root. Tooling should keep image-user scratch directly under `/data` (not inside a runtime directory) and remove it on exit. Leftovers are reclaimed by chown, not removed, so a tool that refuses an existing scratch root (as `mkdir` does) needs the agent to delete it first.

Limits of these work-arounds:

- The reclaim runs only on cold launches. atelet resets the node-local DurableDir and rootfs upper on suspend (after upload), on restore and on actor deletion, before any relaunch. Foreign-owned, non-empty directories present at those points can still fail the reset: suspend returns an error, or deletion stalls in `DELETING` while holding its worker.
- Paths the reclaim does not cover:
  - a killed tool's scratch while the actor stays up;
  - image-user files inside runtime directories, such as archives extracted with their original owners (GNU `tar` as root preserves them, as do `cp -a` and `rsync -a`);
  - the ownership of `/data` itself;
  - the rootfs upper.
- New Sessions start from the golden `Full` snapshot, so the filesystem preparation ran only once, when the golden was created. `/data`'s mode is part of the durable snapshot. Whether `/`'s mode survives a `Full` restore depends on gVisor capturing the root directory's own metadata, and is verified only by live evidence.
- PostgreSQL started through `runuser` inherits the agent's working directory (`/data/workspace`, mode `0700`), so it starts without access to its working directory. Tools that use absolute paths tolerate this; the fix belongs in the tool's wrapper, not in widening the workspace.

These work-arounds stay until upstream Substrate fixes #2035 and #2034. Each is a no-op once the condition it repairs no longer occurs.

Build locally from `go/harness/runtime/payload` with `make build PROVIDER=claude PLATFORM=linux/amd64 IMAGE=runtime-claude:local` (or `PROVIDER=codex`). Publishing and catalog deployment are separate operator steps.

## Trust boundary

Environment selection defaults to denied, independently of ordinary Session authorization. Only service-token mode installs the explicit selection policy and preparer. Insecure and trusted-proxy callers cannot select D, even when their user identity is `mainloop`. The policy requires the verified Mainloop service principal, an allowed Agent, a digest-pinned D from an exact registry authority allowlist, a platform present in the operator payload catalog, and a nonblank policy identity. Internal controller sessions do not bypass this selection boundary.

Configure registry authorities with `developmentEnvironmentRegistries` in `KAGENT_AUTH_SERVICE_POLICY`, or Helm `controller.auth.serviceToken.developmentEnvironmentRegistries`. Entries are exact hosts with optional ports, without schemes or repository paths; an empty list denies selection. The Session service also requests `create` authorization on a `DevelopmentEnvironment` resource named by the selected digest reference. Development-image registration must separately validate ABI requirements, image-declared user, immutable policy identity and grants; those checks remain the trusted Mainloop caller's responsibility.

Local amd64 glibc packaging checks do not prove arm64 execution, live gVisor provisioning, Full snapshot restoration or registry retention. Those require integration and release evidence.
