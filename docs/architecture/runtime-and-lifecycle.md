# Runtime and Lifecycle

An `Session` is PostgreSQL-backed control-plane state exposed through gRPC.
It pins one prepared revision and names one Substrate Actor. It is not a
Kubernetes resource.

## Creation and state

Fresh creation pins the Agent's latest successful runtime revision. Pending or
failed edits leave that revision available; an Agent with no successful revision
cannot create sessions. Creation retries retain the existing session's pinned
revision. Explicit egress grants are applied from the controller's persisted
intended policy before execution. Lifecycle
operations do not read or translate Kubernetes policy.

Namespace reference policies constrain compilation. Failed resolution preserves
the last successful revision for new sessions; existing sessions remain pinned.

The control plane creates a deterministic Actor initially suspended and marks the session
ready after Substrate accepts it. Readiness of the image was already established
while preparing the ate-api ActorTemplate; Session creation does not resume
an Actor merely to probe `/readyz`.

Substrate v1.0.0 requires protocol-specific egress policies and permits
DNS queries only for hostnames matched by those policies. Kagent allows
each configured HTTP(S) origin, preserving its scheme, DNS name, and port, and
replaces credential headers in that destination's deciding rule. Conflicting
protocols on the same host and port are rejected before Actor creation. Literal
IP allowlists are unsupported by this Substrate release; model, MCP, and telemetry endpoints
must use DNS names. Host-based test services use Kubernetes Services and
EndpointSlices to provide those names.

Runtime endpoints must use full DNS names, such as
`kagent-controller.kagent.svc.cluster.local`. DNS search-domain expansion of a
short service name would query a hostname absent from the egress policy. The
Helm chart uses `global.clusterDomain` (default `cluster.local`) for its generated
controller and API tool URLs; custom model, MCP, and telemetry URLs must also
use full names.

Create (including forks), Delete, and controller-owned deadline suspension keep
their current operation UUID and executor claim on the session row. Fork creation loads its
pinned checkpoint from PostgreSQL. Namespace provisioning belongs to the Agent
controller; session creation uses the pinned ActorTemplate's existing namespace.
Read-only preparation may run concurrently, but an atomic execution claim permits
one bounded inline attempt to issue runtime mutations. Network work holds no database
transaction or lock. Completion changes the session atomically and retains its
operation UUID until a later transition supersedes it.

A joined caller observes the current session only while its admitted generation
remains current. A superseded caller gets a conflict and issues no runtime work,
even when the new operation has the same state and kind. There is no historical
lifecycle result archive or pruning requirement. Creation retries return current
session state without requiring an old operation receipt. A2A task storage and checkpoint
reconstruction have their own durable history requirements.

An unclaimed operation can retry preparation; Delete may supersede it. Every
admitted Session deletion enters DELETING. Preparation failure invalidates its
generation but leaves DELETING in place, keeping task admission closed until a
delete retry completes. Explicit deletion requires a client retry; idle deletion
is retried by the expiration worker. After an attempt issues runtime work, errors
retain the operation and resource pins. The attempt releases its execution claim
on return. Each attempt, including automatic suspension, runs under
`substrate.operationTimeout` (default 10m), and its execution claim lasts exactly
as long as that attempt, so a second executor cannot start while the first is
still inside a Substrate call. A crashed executor's claim expires at that bound,
allowing a client retry to claim the same operation with a new executor ID; no
claim renewer or recovery sweep runs. Actor resume, suspend and revert move
snapshots, so they use `substrate.lifecycleCallTimeout` (default 5m); other
Substrate RPCs use `substrate.callTimeout` (default 30s). The controller refuses
to start unless the attempt bound exceeds the lifecycle call bound. A timed-out
call is still an uncertain outcome that a retry reconciles from the Actor.
A stale executor cannot complete or release a newer attempt's claim.

Lifecycle retries inspect the pinned Actor identity and continue Substrate's
reentrant workflows, including completing egress setup for an existing creation.
Already completed creation steps are not repeated. Fork retries must still match
the retained source snapshot. Conflicting Session lifecycle, task, and checkpoint
work remains blocked until the pending operation completes. Releasing an attempt
never clears its durable intent or makes uncertain work an unissued preparation.

Clients must retry the mutation after transient errors; Get only observes it.
If the client stops retrying, the explicit operation remains pending across API
restarts. There is no general automatic lifecycle recovery. The
[client retry contract](../lifecycle-retries.md) covers error codes, deadlines,
creation request IDs, and the limitations of retries and concurrent callers.

Explicit deletion retains an indefinitely kept DELETED session tombstone with its owner,
creation request identity, and final operation UUID. It clears runtime routing,
releases the revision and checkpoint pins, and revokes shares atomically. A fork's
source checkpoint UUID remains as request identity; a generated foreign-key column
pins that checkpoint only while the session is live. Ordinary session/task/share
access excludes deleted sessions. Create/Fork request IDs remain reserved after explicit
deletion and cannot recreate compute. Public Delete still returns NotFound for a
fresh request after deletion; already-authorized joined Delete callers can observe
the final tombstone. No public operation API is introduced.

Session runtime suspension and resumption are platform-managed: idle settlement
suspends the Actor while the Session stays READY, and Substrate ingress resumes
it for the next interaction. There are no public Session suspend/resume controls;
standalone Sandboxes retain their explicit controls. Scheduled-run deadline cleanup
can still suspend a Session internally when no scheduled task can be identified.

Because a sleeping Session stays READY, `GetSession` reports whether its runtime
is awake through `runtime_association`. On each authorized read of a READY
Session with no lifecycle operation, the control plane looks up the Session's
Actor and reports it only when it is still the recorded Actor of the expected
template:
- `generation_id` is a name-based UUID of the Session ID and the Actor UID
- `phase` is `active`
- `current_active` is true only while the Actor is running

An absent association means this read verified nothing, not that the runtime
is asleep. List and other reads never include it. It is an observation, not a
grant: callers fence runtime work with their own checks.

Deletion closes task admission,
stops and deletes the Actor, then tombstones the session. The workflow
entry points are in
[`go/core/internal/service/session`](../../go/core/internal/service/session).
TaskStore writes and automatic idle lifecycle work use durable task boundaries
alongside these explicit lifecycle claims. Checkpoint reservations also block
conflicting task writes and idle lifecycle work.

`DeleteSession` without `force` returns an error if the session has active turns,
outstanding dispatch, or unfinished native cleanup, without entering `DELETING`.
`INPUT_REQUIRED` and `AUTH_REQUIRED` tasks are waiting; their state alone does not
block deletion. Set `force: true` (CLI: `kagent session delete ID --force`) to
bypass these activity checks. Authorization and checks for conflicting checkpoint
or lifecycle work still apply.

Both modes call Substrate `DeleteActor` directly with a UID precondition, without
suspension or a final snapshot. An Actor in `RESUMING` does not need to resume first.

When Substrate reports deletion or actor absence, kagent publishes saved task
outcomes and marks remaining active tasks canceled. These changes and the Session
tombstone commit atomically. Once deletion is admitted, client retries continue
without repeating `force`; new turns remain blocked.

The unreleased schema requires a clean database. Do not overlap older binaries that
can issue lifecycle calls without session-local execution claims. PostgreSQL tests with controlled
Actor responses verify claim ordering, delayed callers, and lost responses; live
Substrate settlement and the complete multi-replica rollout remain acceptance work.

## Idle expiration

Sessions expire after seven days without task activity by default, using the same
bounded deletion workflow as Sandboxes. `config.sessions.idleTtl` in Helm configures
the controller. Values use protobuf durations (`604800s` for seven days,
`2592000s` for thirty); `0s` disables default admission, and negative values are
rejected. `Harness.spec.sessionIdleTTL` overrides the default for every Agent using
that Harness: omitted inherits it, `0s` never expires, and a positive duration
replaces it, even when the controller default is zero. Subsecond durations round up
to the next second. The override is mutable lifecycle policy stored on
`agent_definition`, so edits apply to existing Sessions without changing their pinned
runtime revision. While the Harness cannot be resolved, the last stored override
stays in force. There is no maximum TTL beyond the duration type's range.

Idle time is measured from the later of session creation and the latest stored
A2A event's database timestamp. Forks retain original event timestamps, so their
own creation time gives them a full idle lifetime. Reads, renames, lifecycle calls,
and retried writes or settlement do not extend it. INPUT_REQUIRED/AUTH_REQUIRED
turns use the same idle TTL, so an unanswered input or sign-in request does not
keep a Session alive indefinitely. Submitted and running tasks never expire, even
after that duration. Pending lifecycle operations, dispatch reservations, native
cleanup, claimed suspend work, and checkpoint creation also block expiration.

The TTL applies to Sessions already waiting when the controller is upgraded. The
next sweep may expire a wait older than the configured duration. Expiration ends
the Session through ordinary deletion; it does not resume the agent or publish a
synthetic task timeout.

A leader-only worker scans once per minute in bounded pages. Under the same
PostgreSQL row lock used by task admission, it rechecks the idle clock and active
work, then admits a normal DELETE operation with the durable reason `idle_timeout`
and moves the session to DELETING. A turn admitted after the scan but before the
lock prevents expiration. Once expiration is admitted, new task execution is
fenced, including after preparation failures.
The worker uses the existing Actor deletion workflow and execution claims, so
runtime I/O holds no database locks and overlapping attempts cannot issue the
same generation concurrently. Failed expiration resumes on later sweeps or after
leader replacement; changing or disabling a TTL does not cancel admitted deletion.

After runtime deletion succeeds, the normal delete completion transaction removes
the session, shares, runtime row, and creation receipt. GetSession returns NotFound;
CreateSession or ForkSession with the same caller/request ID can create a fresh
session. Explicit client deletion still retains its tombstone and request ID.

A2A continuation using a missing or expired context ID returns `TaskNotFound`
(JSON-RPC `-32001`, gRPC `NOT_FOUND`), matching continuation using an old task ID.
Ownership-filtered lookup gives the same response for another user's Session;
the response does not distinguish expiry from absence or hidden ownership.
Authorization-check failures still return permission denied. Clients can start a
new conversation after `TaskNotFound` by sending without a context or task ID.
Service-level not-found errors use this same A2A mapping, including missing Agents.
Agent-card HTTP discovery returns 404 for not-found errors and 403 for permission
denials. Starting a new conversation requires an existing, authorized Agent.

The retention rule preserves the existing audit policy: A2A context, tasks, event
history, and explicit checkpoints remain in PostgreSQL. Retained checkpoints can
still be forked after the source session expires and retain their own snapshot
pins. The ordinary Actor deletion workflow removes the session's runtime and
releases its revision/checkpoint references; backend snapshot garbage collection
remains governed by Substrate and retained checkpoint pins. Idle expiration does
not bound audit-history or explicit-checkpoint storage; those need a separate
retention policy. History is no longer accessible through the expired Session API.

`kagent.session.expired` counts completed sweep removals (Prometheus exports
`kagent_session_expired_total`). Each removal logs `expired idle session` at debug
level with `session_id` and `idle_time`.

## Automatic quiescence

The runtime stages a final task update and acknowledges it after native cleanup.
That acknowledgement publishes task state and history atomically, without waiting
for suspension. A Session lifecycle worker independently claims the idle
boundary in PostgreSQL. Both INPUT_REQUIRED/AUTH_REQUIRED and terminal work
suspend the Actor and record the exact durable snapshot. Waiting tasks
are not forkable. The Session stays logically READY, and Substrate ingress
resumes it when another authorized interaction arrives.

Unfinished native cleanup blocks new task writes and explicit lifecycle changes.
After publication, a new turn may supersede idle work before it is claimed. Once
claimed, idle work blocks new execution, explicit lifecycle changes, and checkpoint
capture until its outcome is recorded. The worker performs runtime I/O outside the
database transaction. Successful snapshot references are retried on database failure
without repeating the Substrate operation. Checkpoint creation requires the matching
snapshot and can return FailedPrecondition after task completion while it is pending.

After settlement commits, a bounded, nonblocking in-memory signal wakes the local
workers. Signals coalesce; four workers drain durable claims, handing off a wake-up
before runtime I/O so a burst can use all four workers. PostgreSQL claims coordinate
replicas. Each worker scans at startup and, when idle, every minute by default
(`sessions.quiescenceRecoveryInterval` in controller config). A crash between commit and signaling can
leave an actor running until a surviving replica's next recovery scan; increasing
the interval increases that delay and the time until its snapshot is available.
Ordinary committed settlements do not wait for the recovery interval.

`sessions.quiescenceDelay` (default `0s`, at most `1800s`) delays automatic
suspension after a turn settles, so background processes such as dev servers keep
running for a while. Settlement records the deadline with the database clock, so
acknowledgement retries and controller restarts keep it; configuration changes
affect only newly settled boundaries. Publication is not delayed: the task is
visible immediately. A new turn supersedes unclaimed work during the delay. Claims
check the deadline alongside the dispatch and lifecycle fences, and work that is not
yet due does not count as pending, so idle workers notice it on their next recovery
scan: suspension can land up to one recovery interval after the deadline. The
delay does not wait for background processes to finish. Boundaries published on
an ending runtime use no delay.

The deadline column (migration 8) is nullable, and NULL means eligible now, so an
older controller can still publish boundaries. Older workers ignore deadlines, so
roll out in two steps: deploy the new controller with `quiescenceDelay: 0s`, wait
for every old replica to exit (all replicas run quiescence workers), then raise the
delay. To roll back the binary, set the delay to `0s` first and keep schema 8.

### Runtime activity

A runtime can be in use outside A2A tasks, for example by an attached client.
Its harness reports busy and idle transitions with `TaskStoreService.ReportActivity`,
which only the Session's authenticated Actor may call. An active report holds a
one-minute lease, and the runtime renews it every 20 seconds while busy, so one
lost report does not end it. An idle report ends the lease at once. Reports on a
deleting Session fail with `FAILED_PRECONDITION`.

While a lease runs, task boundaries are not claimable or pending, so a turn that
settles while a client is attached does not suspend the runtime. Once the lease
ends, the runtime becomes eligible `quiescenceDelay` later. A due task boundary is
claimed first, because its claim records the task snapshot. Otherwise a separate
activity claim, fenced like a task claim by dispatch, unpublished or pending task
events, active tasks and checkpoint creation, suspends the Actor if it is still
running and records no snapshot. The Session stays READY either way. A report
after the claim leaves the activity boundary pending, so the next claim
re-evaluates it. Activity claims never expire, and crash recovery and deletion
retire them together with task boundaries.

The latest report also counts as activity for idle expiration, and an unexpired
lease blocks expiration.

The Claude harness reports its SDK runner's busy/idle transitions through
`go/adk/pkg/activity`. The reporter sends nothing until the first transition,
retries transient failures with backoff, and stops after repeated
`PERMISSION_DENIED` until the next transition.

An empty claim is followed by a check for unclaimed work on ready sessions. If
dispatch, checkpoint creation, a lifecycle operation, or a row lock blocks that
work, workers retry within one second until it becomes eligible or is superseded.
Database errors also use this short retry. Sessions that are not READY and claimed
work do not keep the short retry active. Idle workers make one claim attempt and
one pending-work check per recovery interval, rather than querying every second.
Other background workers and connection-pool idle settings still affect whether
PostgreSQL can become idle.

Unclaimed idle work survives API restarts. A claim for possibly issued runtime work
never expires: losing the worker does not prove that the suspend stopped. Uncertain
claims still block new work, but completed results remain readable. The recorded
actor UID is checked before lifecycle calls; a same-name replacement cannot be
adopted implicitly.

`SessionService.RevertSession` lets an authorized caller restore a confirmed
crashed runtime. It accepts active and idle Sessions, but verifies Substrate's
CRASHED state for the recorded Actor UID and pinned template before admitting
recovery. Quiet turns, transport failures, missing Actors and same-name
replacements cannot authorize a revert. A runtime that is not crashed, including
a Session at rest outside READY, has nothing to recover, so RevertSession
returns the Session unchanged. That is also what a retry sees after a revert
whose response was lost. A missing or replaced Actor returns
`FAILED_PRECONDITION`, because no retry restores it. A transport failure returns `UNAVAILABLE`. Only an active
lifecycle operation returns `ABORTED`.

Admission records FAILED/REVERT with a RuntimeCrashed failure under the Session
lock. REVERT is its own `RuntimeOperation`: only a confirmed crash starts one, and
`BeginSessionOperation` can join a REVERT but never start one. The same transaction
publishes a durable terminal result saved before the crash, or fails nonterminal
work absent from the retained snapshot. Waiting tasks captured by that snapshot
remain available. Checkpoint creation excludes competing recovery.

Admission settles waits against the Actor's latest completed durable snapshot,
which is the one Substrate's revert restores. Every suspend kagent issues uses
synchronous durability, so its upload completes before suspension returns, and
a crashed Actor cannot be suspended. No upload can complete between admission
and the revert, so settling at admission settles against what is restored, and
the interrupted turn fails as soon as the crash is confirmed. An asynchronous
suspend would break this and require settling when the revert finishes.

The caller reads the Session's generation before it observes Substrate: its
lifecycle generation, recorded Actor UID, and Actor template, in one statement.
The observed Actor must match that template and UID. Admission takes the read
back, rejects the crash when another lifecycle operation began after it, and
re-checks the Actor UID under the Session lock. A concurrent revert keeps the
Actor UID and returns the Session to READY, so only the generation exposes a
stale observation. The recorded UID changes only through a lifecycle operation,
which also replaces the generation.

An issued idle suspension does not exclude recovery. It cannot finish against the
crashed Actor, and its worker keeps the claim when suspension fails. Admission
retires the claim in its transaction, so a late finish records nothing.

Execution reverts the same Actor to its last durable snapshot without replaying
input. A runtime without a durable snapshot returns to its initial state.
An interrupted revert continues from REVERTING; a lost response can be reconciled
from SUSPENDED. Nothing retries a recovery in the background. An admitted
recovery survives API restarts as a durable REVERT operation, and the client
continues it by retrying RevertSession, as with Create and Delete. While it is
pending, `GetSession` reports FAILED/REVERT with failure reason `RuntimeCrashed`,
and interactions are refused because a lifecycle operation is in progress.

Delete supersedes an admitted recovery even after an attempt issued the revert.
Other issued lifecycle work blocks Delete because it may still create something
Delete would miss; a revert restores the recorded Actor in place and creates
nothing. Substrate serializes the two calls, and a late revert fails against the
deleted Actor. A recovery whose Actor is missing or replaced stays pending, and
each retry returns
`FAILED_PRECONDITION` so the client deletes the Session.

A successful revert returns the Session to READY with the same Actor suspended.
The next authorized interaction resumes it through Substrate ingress. Runtime
changes after the retained snapshot are lost; the discarded turn receives no
checkpoint at that older snapshot. Another revert returns the Session.

```mermaid
sequenceDiagram
    participant Client
    participant Gateway
    participant Actor as Agent runtime
    participant API as TaskStore API
    participant DB as PostgreSQL
    participant Worker as Session lifecycle worker
    Client->>Gateway: authorized send / continuation
    Gateway->>Actor: invoke
    Actor->>API: create and versioned updates
    API->>DB: stage final boundary
    API-->>Actor: committed version
    Actor->>API: settle after native cleanup
    API->>DB: publish task/history atomically
    API-->>Worker: nonblocking local wake-up
    Actor-->>Gateway: final event
    Gateway->>Actor: close observer connection
    Gateway->>DB: observe publication
    Gateway-->>Client: current public task
    Note over Worker,DB: Idle lifecycle runs independently of the client response
    Worker->>DB: claim idle boundary unless new execution superseded it
    Worker->>Actor: suspend at the template's preferred fidelity
    Actor-->>Worker: suspended Actor and durable snapshot
    Worker->>DB: record snapshot and finish idle claim
```

## Runtime boundaries

- Port `8083` serves native gRPC, gRPC-Web, A2A, authenticated MCP, and health.
- Actor A2A gRPC is private on port `80`.
- Runtime readiness is private HTTP `/readyz` on port `8081`.
- ate-api defaults to `dns:///api.ate-system.svc:443`.
- Actor ingress defaults to `https://atenet-router.ate-system.svc:443` and uses
  the same CA bundle and rotating controller client certificate as ate-api.
  Substrate's router must allow the controller's SPIFFE identity; the installer
  configures this for both A2A and sandbox guest access.

Clients never receive Actor addresses. The gateway derives and dials them through
the private atenetwork router.

Every Actor mounts a Substrate `DurableDir` at `/data`. Harnesses keep private
state there—local framework state, workspaces, and downloaded assets that must
survive Actor replacement. This state is runtime-private; public task history
remains in PostgreSQL.

The ADK's built-in file and shell tools use `/data/workspace`, shared by the root
agent and subagents in one Actor. Workspace files survive VOLUMES snapshots, resume,
and checkpoint forks. Skills are discovered independently; the workspace has no
session-specific directories or mutable skills symlinks. The workspace is created
on first use, with no predefined upload or output directories. File tools accept
normal relative and absolute paths inside the sandbox; Substrate owns isolation.

Each bash invocation starts a fresh shell with the Actor environment. The working
directory and configured skills directory are added to `PYTHONPATH`. Commands use
a 60-second default timeout; `timeout_seconds` can select up to 600 seconds.
Stdout and stderr are combined, captured up to 64 KiB, and returned with an
explicit truncation marker. Large results should be redirected to workspace files.
Commands and their process groups are stopped when a call finishes, times out,
or is canceled. Background processes are not durable work and have no process
handle API in the bash tool. With the default `Data` quiesce scope, suspending an Actor preserves files, not running
commands. Command payloads and output are not logged by the shell helpers.

### Quiesce snapshot scope

`spec.substrate.snapshotPolicy.onQuiesce` on a Harness or SandboxTemplate
selects the ActorTemplate's preferred snapshot fidelity:

| onQuiesce | Preferred fidelity | On resume |
| --- | --- | --- |
| `Data` (default) | VOLUMES | Fresh containers from the OCI image with the saved durable directories |
| `Full` | MEMORY | Processes running at suspension continue where they stopped |

Only processes that outlive a turn benefit. The Claude harness ends its whole
native process tree at the end of every turn, including commands that Bash
detached with `setsid`, so a dev server started from Claude's Bash tool does not
survive a turn with either scope. The kagent bash tool likewise stops each
command's process group when the call finishes. `Full` keeps standalone sandbox
guest processes and any process the runtime itself keeps between turns.

The template applies to every suspension of the Actor, whether the task is
waiting for input or finished. Harnesses recover pending approvals and user
input from their durable state with either scope; sandbox guests keep their
process registry only with `Full`. `Data` hashes as an unset field, so
existing revisions keep their digests; `Full` produces a new revision.

Substrate v1.0.1 supports MEMORY on both the gVisor (`runsc checkpoint`) and
microVM workers. The preferred fidelity is a ceiling, not a guarantee:

- Substrate may package less than MEMORY, for example under host memory
  pressure.
- Resuming on a worker whose runtime cannot restore the snapshot falls back
  to ROOTFS.
- An Actor whose template was replaced restores only VOLUMES.

So a Full Harness must still tolerate a cold start; processes that matter
should be restartable from files.

Kagent selects the highest snapshot generation and accepts its object-storage
URI only after its durable status is COMPLETED. A pending generation cannot
fall back to an older checkpoint. Suspension requires a durable snapshot at
VOLUMES, ROOTFS or MEMORY fidelity. Only VOLUMES checkpoints can be forked, so
a fork never carries the source session's process memory or runtime identity
into the new session; checkpoints of a `Full` Session are retained but return
`FAILED_PRECONDITION` from `ForkSession`. Substrate restores an Actor created
from a Tag at the Tag's fidelity under the same ActorTemplate, and offers no
way to restore less, so a fork of a MEMORY checkpoint would resume the source
Session's processes and identity.

Codex and Claude record failed deferred continuations as `abandoned` in their
existing input journals. Admission rejects abandoned or restored unfinished
execution phases with an error requiring a new session, instead of restoring a
pending interaction for a failed task. Failures known to precede execution keep
the existing retry behavior. This rule is limited to durable deferred input;
general crash recovery and remote tool outcome reconciliation are follow-up work.

The Go ADK opens and migrates its SQLite session store before readiness, but
retains no idle database connections. MEMORY and golden restores preserve guest
memory while rematerializing `/data`, so a connection opened before the snapshot
can retain a stale file identity and reject writes with `SQLITE_READONLY_DBMOVED`.
Closing connections when returned to the pool keeps quiescent snapshots free of
database handles; each later operation opens the current backing file.

## Runtime revision cleanup metrics

GC uses the controller's shared OpenTelemetry provider and configured OTLP export.
Prometheus scraping is opt-in through `controller.metrics.enabled`.

| OTel metric | Instrument / unit | Prometheus name | Meaning |
| --- | --- | --- | --- |
| `kagent.runtime_revision.gc.pending` | Observable integer gauge / `{revision}` | `kagent_runtime_revision_gc_pending` | Eligible persisted revisions from the last successful discovery. No application attributes. |
| `kagent.runtime_revision.gc.duration` | Histogram / `s` | `kagent_runtime_revision_gc_duration_seconds` | Each discovery or collection attempt, including claim, Substrate read/delete, and finalization. `kagent.gc.stage=discovery\|collection`; `error.type` only on failure: a Substrate gRPC code name or `_OTHER`. Parent cancellation is excluded; operation deadlines count as failures. |

Pending is absent before successful discovery, on standby replicas, and after GC
stops. Do not fill absence with zero: zero means a successful empty discovery.
Discovery errors retain the last count. Scrapes only read the cache; restart
reconstructs pending from PostgreSQL and resets process-local histogram totals.

- **Growing pending:** compare attempt rates, failure ratios, and latency on the
  active controller before diagnosing churn versus slow or failing cleanup.
  Let GC retry; never bypass reference/UID protections or clear deletion markers.
- **Rising failure ratio or latency:** use reset-aware `rate` on histogram
  `_count` (failed attempts have `error_type`), grouped by `kagent_gc_stage`,
  and `_bucket` quantiles. Discovery errors point to the database; collection
  errors require checking the bounded error type and logs (`revision`,
  `actor_template_atespace`, `actor_template_name`, `error`) to identify the
  failing dependency and repeated same-object failures.

## Inventory pagination

Actor and worker lists filter by current authorization, watched namespaces, and
request scope before filling a public page. They look ahead for one more authorized
row, and issue a continuation only when it exists. Public cursors contain the last
returned identity and request scope, never an upstream cursor that could name a
filtered-out row.

The Substrate adapter translates that identity into the pinned v1.0.0 keyset
token format: actors use atespace/name globally or name within an atespace; workers
use globally unique names. The encoder is version-dependent and its contract tests
must be checked when upgrading Substrate. Continuations resume directly after the
boundary without requiring it to still exist. Authorization is evaluated anew;
cursors grant no access. Watched atespaces are traversed in sorted order.

Only the authorized page and an upstream batch are retained. Lookahead can be read
again on the next request, and sparse authorization can require many backend reads,
but previous public pages are not rescanned. Each public list request has a
10,000-row and 100-backend-page budget shared across atespaces
and a 10-second deadline (or the caller's earlier deadline). The first batch
requests `limit + 1` rows; later batches request up to 1,000 rows, capped by the
remaining row budget. This applies once per request, across atespaces. All fetched
rows count, including denied rows, lookahead, and unused rows within a batch. Exhaustion returns an explicit `ResourceExhausted` or
`DeadlineExceeded` inventory error. Summary scans retain their separate
10,000-page walk bound. Non-advancing token checks also bound failed scans.
Failed scans return no partial page
or continuation, and backend error details remain in server logs.

## Git workspace bootstrap

`CreateSessionRequest.workspace` (`repo`, optional `ref`, `branch`, `depth`) asks for a
repository checkout before the first turn. It is stored on the Session and is accepted
only when the repository host is one of the revision's Git origins (`spec.git.origins`);
the check runs in the same transaction that pins the revision, so a rejected request
creates nothing. Sessions without a workspace behave as before. A Harness with
`spec.git` requires a harness image built with this bootstrap: the adapter
configuration rejects unknown fields, so an older image fails at startup.

With `readProxyOrigin` configured, the shared Git policy accepts only HTTPS
GitHub owner/repository identity. It derives a normalized `/owner/repository.git`
path under the exact trusted read listener and optional distinct push listener.
Escaped separators, traversal, extra components and alternate authorities are
rejected. Session and TaskStore workspace identity and durable marker `repo`
remain the caller's canonical HTTPS repository. Proxy origins are never accepted
as workspace identity.

The checkout sets `remote.origin.url` to the derived read URL and
`remote.origin.pushurl` to the push URL, or the same read URL when push is absent.
It writes the inert Authorization placeholder for each exact listener in local
Git config before fetching and disables redirects. Missing capabilities leave
only that placeholder; the listener must deny the request. A read-only profile
routes push attempts to the read listener, which must refuse receive-pack. No
failure selects direct GitHub. Existing direct standalone/development checkout
is selected only when both compiled proxy fields are absent.

Executor construction validates config and the native CLI version without
reading the workspace or fetching Git. Reference-only Session creation can
therefore finish READY before Git values exist. Creation leaves the Actor
SUSPENDED, and READY Resume alone is a no-op. An owned Suspend/Resume sequence
can warm compute without a turn; GetSession's current RUNNING association is
the subsequent identity observation. Capability publication and installed
runtime qualification remain the caller's responsibility.

The Codex and Claude adapters wrap their runner with a bootstrapper. Before each turn it
checks for the done marker `/data/.kagent/workspace-bootstrap.done`. When it is absent
it reads the workspace through the runtime-authenticated `TaskStoreService.GetWorkspace`
call, which resolves only the calling Session. It then clones into `/data/workspace`:

- `ref` may be a branch, a tag, or a full commit SHA; an empty ref selects the remote
  default branch. `depth` 0, which an omitted depth sends, clones full history, so a
  feature branch keeps a merge-base with `origin/<default>`. A positive `depth` is
  shallow and may have no merge-base. A non-empty `branch` is created or reset from
  the checked-out ref.
- When the Harness has a credential, a placeholder `Authorization` header is written to
  the repository-local `http.https://<host>/.extraHeader`. It is never written to global
  Git configuration, because `~/.gitconfig` does not survive a Data snapshot, while
  `/data` does. The gateway replaces the placeholder in flight. The runtime holds no
  token.
- Two markers record progress in `/data/.kagent`, outside the workspace, so nothing an
  agent does in the workspace (`git init`, a fresh clone, `rm -rf .git`) can forge or erase them.
  `workspace-bootstrap.started` is written before the first change. `workspace-bootstrap.done`
  is written last, and then `started` is removed. Both are written atomically and hold no
  credentials. A failed attempt leaves `started`, so the next turn redoes it.
- `done` present: the bootstrap does nothing, whatever the workspace holds, so resumes
  are idempotent and an agent's own `.git` and commits are never replaced.
- `started` without `done` is our own interrupted attempt. Its `.git` is removed and the
  checkout is redone with `git checkout -f`, because the first attempt may already have
  written files. Other files outside `.git` are left alone.
- Neither marker but a `.git` present: the repository is not ours (an agent created it, or
  the markers were removed). The bootstrap fails with a clear message and never deletes
  it.

A failed bootstrap does not run the agent. The turn ends as a failed task whose message
names the cause (ref not found, authentication, unreachable host, timeout, invalid ref
name), and the next turn retries. The bootstrapper also enforces the Harness origins, so
a Session row that bypassed create-time validation still cannot reach another host.

## Workspace service

`workspace.service_command` optionally declares a workspace service: an argv for a
long-running process, such as a development server, that runs in the checkout. It is
stored with the Session's workspace and returned by `TaskStoreService.GetWorkspace`.
It is fixed at creation and is part of the workspace in preparation requests and
assignments.

A resume restarts the runtime process: `/data` is kept, and every process is gone. So
the Codex and Claude runtimes start the service once per process, without waiting for
a turn, and a Session woken only by traffic to the service gets it back:

1. The process waits until it serves a Session. It may resume from a golden memory
   snapshot taken before it had a Session identity, so it never reads the workspace
   at startup.
2. It reads the workspace once. A Session without a command gets no status file, and
   one left by an earlier start is removed.
3. It waits for the checkout and any preparation exactly as a turn does
   (`Bootstrapper.Ready`), retrying with capped backoff.
4. It starts the command in `/data/workspace` with the environment the agent's tools
   get. The first element is resolved through `PATH`. The service is not restarted
   when it exits.

The harness stays the only process owner. It runs the service under a supervisor,
which is the harness binary re-executed in a hidden supervisor mode. The supervisor
makes itself a child subreaper, so every process of the service stays in its
subtree, even one that daemonizes with `setsid` or a double fork. It also reaps those
processes. The Claude process driver claims every other descendant of the harness;
it takes its census under the service's lock and excludes the subtree of each live
supervisor, identified by PID and kernel start time. On SIGTERM the supervisor sends
SIGTERM to the whole service, then SIGKILL after 10 seconds. The supervisor exits
once no process of the service is left.

The agent sees the service in `/data/workspace-service`:

- `status.json` holds `command`, `state` (`starting`, `running`, `exited` or
  `failed`), the supervisor's `pid`, the start and exit times, and the exit code.
  `state` stays `running` while any process of the service lives, even after the
  command's own process has exited.
- `log` holds the service's stdout and stderr. When it would pass 8 MiB it is
  rotated to `log.1`, so the output uses at most 16 MiB of `/data`.

The harness adds a short note to the agent's instructions whenever the Harness has
Git, so the agent knows to check the status file before it starts a server of its
own. Until a restart action exists, an agent restarts the service by sending SIGTERM
to the `pid`, waiting for `exited`, and running the command itself. That copy lasts
only until the runtime next stops; the next start brings back the declared service.

## Workspace preparation

Some Sessions need setup on their current runtime before they can accept turns.
That is a Session with a `workspace` and a selected `development_environment`
whose revision routes Git through both trusted proxies (`readProxyOrigin` and
`pushProxyOrigin`). Its creator prepares it once with `PrepareSessionWorkspace`.

The request repeats the Session's creation request ID, revision, workspace,
environment and composition. It also names the `generation_id` and `actor_uid`
the caller read from `runtime_association`, and a setup profile (`supervisor`,
`child` or `agent`) with its `setup_digest`.

The control plane refuses in these cases:

| Condition | Code |
| --- | --- |
| A share, or a Session that does not need preparation | PERMISSION_DENIED |
| A setup digest that does not match its profile | INVALID_ARGUMENT |
| The same `action_id` with other inputs | ALREADY_EXISTS |
| The runtime is stale or not running, or the Session lacks exactly one read and one push proxy credential | FAILED_PRECONDITION "Current prepared runtime identity is unavailable" |

When the runtime is not running, the call also wakes it (egress policy first,
then resume). In every stale case the caller re-reads the Session and retries.
The specific cause is logged once per Session and generation.

An accepted request records one row in `session_workspace_preparation`. It
stores the request bytes and the runtime's assignment, and returns a `pending`
receipt. The assignment holds the request's action and setup, the Session's
workspace and composition, the observed Actor, and digests of the revision's
bootstrap config and MCP servers. Retrying with identical bytes returns the
same receipt.

The runtime settles the preparation:

1. `TaskStoreService.GetWorkspace` returns the assignment, but only to the Actor
   it names and only while it is pending. The first read records the issue;
   later reads return the same bytes. These reads take no lock, because the
   runtime calls them before every turn.
2. `CompleteWorkspacePreparation` accepts only that exact, already issued
   assignment from that Actor:
   - its first report moves the row to `confirmed` or `definite-failure`
   - repeating the same report succeeds
   - a different report is refused with ABORTED

`GetSession` reports the receipt as `workspace_preparation`. A receipt for an
earlier Actor is marked `historical`.

Until the preparation is `confirmed`, the store refuses new runtime tasks for
the Session. The check runs in the same transaction that creates the task. A
`definite-failure` keeps turns refused; the caller deletes the Session.

### Runtime workspace preparation

When the control plane requires preparation for a Session, the runtime prepares
it instead of bootstrapping on the first turn. The runtime reads its assignment
through `TaskStoreService.GetWorkspace` once a second, on the same authenticated
channel, and stops polling when the response reports that the Session requires
no preparation. Turns that arrive while preparation is required but not ready
end with "Workspace preparation is incomplete." and never reach the agent.

An assignment carries an exact commit `ref` and a `branch`, together with the
digests of the fixed setup it installs:

- `setup_digest` is the SHA-256 of the profile's standing context
  (`supervisor`, `child` or `agent`), the fixed role text in
  `go/api/workspace`. The text must not change byte for byte, because the
  control plane hashes the same text.
- `config_digest` and `mcp_digest` hash the bootstrap bundle's `config_json`,
  re-marshaled as a top-level map, and its `mcp_servers`. The MCP server must
  belong to the same Mainloop instance as the Git policy's read proxy, matched
  by URL origin, so a Session cannot pair one instance's Git credentials with
  another instance's MCP identity.

The runtime first writes the effect to `native-preparation.issued` in
`/data/.kagent` with an exclusive create and an fsync. Only then does it clone
through the existing bootstrap. It never adopts or resets a checkout it didn't
make. An issued action is only ever observed again, never redone, including
after a crash.

After the checkout, the runtime reads it back. HEAD must equal `ref` on
`branch`, and the local Git config must hold exactly the fixed proxy transport
plus `git init`'s own defaults. The native setup installer then adds the
standing context to the native hook, and setup is refused if native history
already exists:

| Harness | Native hook |
| --- | --- |
| Claude | `append_system_prompt` |
| Codex | `developer_instruction` |

The runtime persists the result to `native-preparation.result` before reporting
it with `CompleteWorkspacePreparation`. It writes `native-preparation.ack`
after the report succeeds, so a lost reply resends the original bytes.

`native-setup.json` records the installed profile and digests. A restarted
runtime restores the standing context from that record, and checks the
materialized native files against it before it rewrites any of them. A Claude
runtime also checks its MCP, settings and system prompt files. A Codex runtime
checks its rendered `config.toml`. Each turn of a prepared Session re-observes
the installed setup before it runs.
