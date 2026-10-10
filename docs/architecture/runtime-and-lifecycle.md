# Runtime and Lifecycle

An `Session` is PostgreSQL-backed control-plane state exposed through gRPC.
It pins one prepared revision and names one Substrate Actor. It is not a
Kubernetes resource.

## Creation and state

Creation selects the latest successful revision for the Agent, creates a deterministic Actor initially suspended, and marks the session
ready after Substrate accepts it. Readiness of the image was already established
while preparing the ate-api ActorTemplate; Session creation does not resume
an Actor merely to probe `/readyz`.

Substrate v0.4.0-alpha1 requires protocol-specific egress policies. Kagent allows
each configured HTTP(S) origin, preserving its scheme, DNS name, and port, and
replaces credential headers in that destination's deciding rule. Conflicting
protocols on the same host and port are rejected before Actor creation. Literal
IP allowlists are unsupported by this Substrate release; model, MCP, and telemetry endpoints
must use DNS names. Host-based test services use Kubernetes Services and
EndpointSlices to provide those names.

Create (including forks), explicit Suspend, Resume, and Delete keep their current
operation UUID and executor claim on the session row. Fork creation loads its
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
session state; already-at-target Suspend/Resume requests are successful no-ops.
Neither requires an old operation receipt. A2A task storage and checkpoint
reconstruction have their own durable history requirements.

An unclaimed operation can retry preparation; Delete may supersede it. Every
admitted Session deletion enters DELETING. Preparation failure invalidates its
generation but leaves DELETING in place, keeping task admission closed until a
delete retry completes. Explicit deletion requires a client retry; idle deletion
is retried by the expiration worker. After an attempt issues runtime work, errors
retain the operation and resource pins. The attempt releases its execution claim
on return. A crashed executor's claim expires after at most two minutes, allowing
a client retry to claim the same operation with a new executor ID. Each attempt
uses a context bounded by that interval; no claim renewer or recovery sweep runs.
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

Explicit suspend and resume update the logical lifecycle state. Deletion closes task
admission, stops and deletes the Actor, then tombstones the session. The workflow
entry points are in
[`go/core/internal/service/session`](../../go/core/internal/service/session).
TaskStore writes and automatic idle lifecycle work use durable task boundaries
alongside these explicit lifecycle claims. Checkpoint reservations also block
conflicting task writes and idle lifecycle work.

The unreleased schema requires a clean database. Do not overlap older binaries that
can issue lifecycle calls without session-local execution claims. PostgreSQL tests with controlled
Actor responses verify claim ordering, delayed callers, and lost responses; live
Substrate settlement and the complete multi-replica rollout remain acceptance work.

## Idle expiration

Sessions expire after seven days without task activity by default, using the same
bounded deletion workflow as Sandboxes. `controller.sessionIdleTTL` in Helm sets
`KAGENT_SESSION_IDLE_TTL` on the controller. Values use Go durations (`168h` for seven days,
`720h` for thirty); zero disables default admission, and negative values are
rejected. `Harness.spec.sessionIdleTTL` (including inline Agent harnesses) overrides
the default: omitted inherits it, `0s` never expires, and positive durations replace
it. Subsecond durations round up to the next second. The mutable TTL is stored on
`agent_definition`, so edits affect existing Sessions without changing their pinned
runtime revision. A positive override works even when the controller default is zero.
There is no maximum TTL beyond the duration type's range.

Idle time is measured from the later of session creation and the latest stored
A2A event's database timestamp. Forks retain original event timestamps, so their
own creation time gives them a full idle lifetime. Reads, renames, lifecycle calls,
and retried writes or settlement do not extend it. Running tasks and
INPUT_REQUIRED/AUTH_REQUIRED turns never expire, even after that duration. Pending
lifecycle operations, dispatch reservations, native cleanup, claimed pause/suspend
work, and checkpoint creation also block expiration.

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
for automatic quiescence. `KAGENT_QUIESCE_DELAY` (Helm
`controller.quiesceDelay`) delays automatic pause or suspension after publication.
It defaults to `0` and is clamped to `0`–`30m`. Publication records the deadline
in PostgreSQL using the database clock; acknowledgement retries and controller
restarts retain it. Configuration changes affect newly settled boundaries only.
A new turn supersedes unclaimed work during the delay. The atomic quiescence
claim checks the deadline alongside the existing dispatch and lifecycle fences.
The delay grants background processes time to run until eligibility; it does not
wait for those processes to finish.

Migration 7 keeps deadlines nullable: an older controller can still publish a
boundary without a deadline, which means immediate quiescence eligibility.
Older workers do not inspect deadlines, so enable the delay in separate GitOps
steps:

1. Deploy the new controller with `controller.quiesceDelay: "0"`.
2. Verify that all old controller replicas have exited. Every replica runs
   quiescence workers, regardless of leader election.
3. Set `controller.quiesceDelay` to the desired duration, such as `15m`.

For a binary rollback, first set the delay to `"0"` and finish that configuration
rollout, then roll back the controller image. Keep schema 7 in place; older
writers remain compatible with it.

A Session lifecycle worker independently claims the idle
boundary in PostgreSQL. INPUT_REQUIRED/AUTH_REQUIRED pauses the actor on its node;
terminal work suspends it and records the exact external snapshot. Waiting tasks
are not forkable. The Session stays logically READY, and Substrate ingress
resumes it when another authorized interaction arrives.

The snapshot scope for terminal work comes from `spec.substrate.snapshotPolicy.onQuiesce`
on the Harness. `Data` (the default) snapshots only `/data`; the actor starts fresh from
the OCI image with `/data` restored on the next request, so processes such as a
dev server do not survive. `Full` also captures guest memory and the root filesystem, so those
processes keep running across suspend and resume. Full snapshots are larger and
slower to take and restore, and the choice is part of the runtime revision, so
changing it creates a new ActorTemplate. Pausing for INPUT_REQUIRED/AUTH_REQUIRED
is always Full, regardless of this field. Forking requires a Data checkpoint, so a
checkpoint whose snapshot is Full cannot be forked.

The workflow reads `SnapshotConfig.OnCommit` from the prepared revision's pinned
ActorTemplate and verifies its name, atespace, and UID. Automatic quiescence and
explicit suspension that captures runtime state require the returned external
snapshot to match that scope and template UID. An unclaimed explicit suspension
of an already SUSPENDED Actor verifies its lifecycle identity and atomically
settles without claiming or issuing runtime work. A newly created Actor may have
no snapshot or borrow golden FULL state even under an OnCommit DATA policy.
Once a suspension has been claimed, its durable executor ID marks possibly issued
capture. Every retry validates the captured snapshot, even if the Actor is now
SUSPENDED; invalid capture retains the operation fence. Completed task boundaries
always retain strict Quiesce validation. Checkpoint Tags preserve the recorded scope;
they accept either DATA or FULL. Same-Actor resume delegates snapshot restoration
to Substrate. The DATA restriction applies to creating a new Actor from a
checkpoint, because a FULL fork would restore the source process state and
runtime identity.

Unfinished native cleanup blocks new task writes and explicit lifecycle changes.
After publication, a new turn may supersede idle work before it is claimed. Once
claimed, idle work blocks new execution, explicit lifecycle changes, and checkpoint
capture until its outcome is recorded. The worker performs runtime I/O outside the
database transaction. Successful snapshot references are retried on database failure
without repeating the Substrate operation. Checkpoint creation requires the matching
snapshot and can return FailedPrecondition after task completion while it is pending.

Unclaimed idle work survives API restarts. A claim for possibly issued runtime work
never expires: losing the worker does not prove that the suspend stopped. Uncertain
claims still block new work, but completed results remain readable. The recorded
actor UID is checked before lifecycle calls; a same-name replacement cannot be
adopted implicitly.

### Recovering a claim held after snapshot scope rejection

An older controller could successfully suspend an Actor with an `onQuiesce: Full`
policy, then reject its FULL snapshot and leave its task boundary claimed. Merely
restarting the controller does not release that claim. No claim-expiry migration
is required for the scope fix, and a timeout does not authorize takeover.

For this specific failure, an operator must:

1. Hold dispatch and lifecycle traffic for the affected Sessions, including queued
   requests, until step 6 verifies their original boundaries. Clearing the executor
   reopens admission before the idle worker claims; a new task could supersede
   unclaimed idle work without recording its snapshot.
   Stop all controller replicas through GitOps and verify their workers have
   exited. Do not release claims while a worker could still issue runtime work.
2. Read the affected Session's pinned revision and generation, then observe the
   Substrate Actor and ActorTemplate. Require the recorded Actor UID, SUSPENDED
   state, a nonempty external snapshot URI, and the pinned template UID/scope.
   A missing/replaced Actor, SUSPENDING state, or mismatched scope is unresolved
   work; leave its claim fenced. Record the observed snapshot atespace, URI, and
   scope (DATA or FULL). Correlate the terminal task ID and event sequence with the
   original scope-rejection log; these identify the boundary to recover.
3. Read the claim using the affected Session ID (psql variable `session_id`).
   Confirm its task ID and sequence identify the original rejected boundary:

   ```sql
   SELECT e.task_id, e.sequence, e.quiescence_executor_id
   FROM session_task_event e
   JOIN session_record s ON s.history_id = e.history_id
   WHERE s.id = :'session_id'::uuid
     AND s.state = 'RUNTIME_STATE_READY'
     AND s.operation = 'RUNTIME_OPERATION_NONE'
     AND e.published AND e.quiescence_pending = TRUE
     AND e.quiescence_executor_id IS NOT NULL;
   ```

4. Release only that executor's claim, using the observed `sequence` and
   `executor_id` as psql variables. Require exactly one returned row:

   ```sql
   UPDATE session_task_event e SET quiescence_executor_id = NULL
   FROM session_record s
   WHERE s.history_id = e.history_id AND s.id = :'session_id'::uuid
     AND s.state = 'RUNTIME_STATE_READY'
     AND s.operation = 'RUNTIME_OPERATION_NONE'
     AND e.sequence = :'sequence'::bigint
     AND e.quiescence_executor_id = :'executor_id'::uuid
     AND e.published AND e.quiescence_pending = TRUE
   RETURNING e.sequence;
   ```

5. Start the fixed controller through GitOps. Its idle worker claims the same
   boundary and validates the already suspended snapshot without another suspend
   request. It stores the snapshot and clears `quiescence_pending` atomically.
6. Keep traffic held while verifying that exact terminal task and event sequence.
   Using the recorded `task_id`, `sequence`, `snapshot_atespace`, `snapshot_uri`,
   and `snapshot_scope` as psql variables, require exactly one returned row:

   ```sql
   SELECT e.task_id, e.sequence, t.history_sequence, e.quiescence_executor_id
   FROM session_record s
   JOIN session_task_event e ON e.history_id = s.history_id
   JOIN session_task t ON t.history_id = e.history_id AND t.id = e.task_id
   WHERE s.id = :'session_id'::uuid
     AND e.task_id = :'task_id' AND e.sequence = :'sequence'::bigint
     AND e.published AND e.quiescence_pending = FALSE
     AND t.history_sequence = e.sequence
     AND e.snapshot_atespace = :'snapshot_atespace'
     AND t.snapshot_atespace = e.snapshot_atespace
     AND e.snapshot_uri = :'snapshot_uri' AND t.snapshot_uri = e.snapshot_uri
     AND e.snapshot_content_scope = :'snapshot_scope'
     AND t.snapshot_content_scope = e.snapshot_content_scope;
   ```

   `quiescence_pending=FALSE` alone is insufficient: superseded idle work also has
   that value without a snapshot. Successful finish retains the new executor ID;
   it need not be NULL. Restore traffic only after the original task/event's exact
   snapshot and matching `history_sequence` are recorded. Then retry SuspendSession
   or checkpoint creation.

This procedure changes neither task history nor snapshot scope. It is an operator
recovery procedure, not an automatic adoption API. The controller continues to
reject mismatched snapshots and preserves the claim when verification fails.

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
    Actor-->>Gateway: final event
    Gateway->>Actor: close observer connection
    Gateway->>DB: observe publication
    Gateway-->>Client: current public task
    Note over Worker,DB: Idle lifecycle runs independently of the client response
    Worker->>DB: claim idle boundary unless new execution superseded it
    Worker->>Actor: pause or suspend
    Actor-->>Worker: settled native boundary
    Worker->>DB: record snapshot and finish idle claim
```

## Runtime boundaries

- Port `8083` serves native gRPC, gRPC-Web, A2A, authenticated MCP, and health.
- Actor A2A gRPC is private on port `80`.
- Runtime readiness is private HTTP `/readyz` on port `8081`.
- ate-api defaults to `dns:///api.ate-system.svc:443`.

Clients never receive Actor addresses. The gateway derives and dials them through
the private atenetwork router.

Every Actor mounts a Substrate `DurableDir` at `/data`. Harnesses keep private
state there—local framework state, workspaces, and downloaded assets that must
survive Actor replacement. This state is runtime-private; public task history
remains in PostgreSQL.

Templates capture Full snapshots when paused; suspension captures the `onQuiesce`
scope (Data by default).
Substrate v0.4.0-alpha1 resumes a Data snapshot by starting fresh containers from
the OCI image with the saved durable directories. Data restores no longer combine
Golden memory with the Actor's saved data.

The Go ADK opens and migrates its SQLite session store before readiness, but
retains no idle database connections. Full and golden restores preserve guest
memory while rematerializing `/data`, so a connection opened before the snapshot
can retain a stale file identity and reject writes with `SQLITE_READONLY_DBMOVED`.
Closing connections when returned to the pool keeps quiescent snapshots free of
database handles; each later operation opens the current backing file.

The Claude harness owns one native process per turn, retaining that process
across human approval waits. Claude's `result` closes an iteration; background
completion can start further activity in the same process. The harness consumes
further iterations until process exit or a driver-owned 120-second post-result
grace expires, returning only the last result. A separate two-hour active
execution ceiling ends the turn as a failure. Both limits are configurable in
harness JSON, exclude approval waits, and apply inside the SDK's detached
execution. Explicit cancellation remains canceled. Linux child subreaping and
`/proc` ancestry tracking let cleanup kill and reap detached descendants. Signals
use pidfds opened before rechecking the observed identity; cleanup never signals
a numeric process group or retained leader PID. Adopted children are reaped
through `waitid(P_PIDFD)`, while `exec.Cmd.Wait` reaps the native leader. The
dedicated Actor owns one native tree at a time and starts no unrelated children
during a turn. Before launching Claude, the harness rejects pre-existing
descendant trees: their later orphans lose the ancestry needed to distinguish
them from native work. Setup subprocesses must finish and be reaped first.
Missing pidfd, subreaping, or `/proc` support rejects execution. Headless harness configuration
disables native background tasks and scheduling by default and removes wakeup,
monitor, cron, and remote scheduling tools. Workspaces can therefore pause
between turns without depending on later native wakeups. See the
[Claude harness contract](../../go/harness/claude/README.md#turn-completion-and-background-work).

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
