# Runtime generation capability identity

TaskStore authenticates a hidden per-generation capability, independent of the
control service bearer. The actor sends `x-kagent-runtime-token:
gateway-injection-required`; the trusted gateway replaces that placeholder with
a 256-bit random token from an immutable Kubernetes Secret. Projected name/UID
files are routing hints, never authentication. All six RPCs require one canonical
64-character lowercase hexadecimal header, an active server-side digest binding,
a fresh uncached Substrate Actor read with matching name/atespace/UID and RUNNING
state, and request Session/task ownership. Control credentials and unsigned actor
metadata are refused. Runtime headers cannot authorize control APIs.

## Durable identity and creation

New Session inserts mark generation eligibility. Migration leaves existing rows
ineligible; a current UID lookup cannot enroll them. No legacy names are accepted
by the SDK. The private ledger permanently reserves a random generation actor
name (`session-<Session UUID>-<16 random hex characters>`) and immutable credential
URI before external effects. Records have no cascading Session foreign key.
Revocation retains names, URIs and digests; garbage collection must never remove
these reservations. Ledger backups and restore/reconciliation are security state.

Creation proceeds through allocated, secret-issued, actor-issued, bound and
active phases. Secret plaintext exists transiently in the trusted controller and
private immutable Secret, never actor environment/files, revision payloads,
public Session responses or snapshots. Only the original allocation can create
its Secret. A lost Secret reply is reconciled by uncached lookup, immutable flag,
generation labels and original digest. Missing original material holds issuance;
retries never rotate identity or overwrite/adopt a foreign Secret.

Actor issuance is recorded before CreateActor/CreateActorFromTag. Creation is
suspended, and only a successful response carrying the exact identity and DATA
snapshot (when applicable) can pin its first UID. An unknown Actor creation
outcome remains held as actor-issued. Even a matching current name/template is
insufficient for adoption. Policy installation precedes active authority. A
retry of a bound generation requires its pinned UID; policy equality is checked
by the existing immutable-policy mechanism. First creation remains suspended,
as before; same-UID lifecycle resume verifies private credential availability
and the exact immutable policy plus active database binding before applying
runtime effects. Actor UID changes or missing Actors hold the
original generation; no replacement token is issued under its old name.

The controller revokes database authority before managed Actor deletion, then
cleans up the Secret. Substrate owns policy cleanup with Actor deletion. A
post-cutover DATA fork creates a new Session and ledger-issued name/URI/token;
source authority is never inherited. A fresh eligible Session deleted before
ledger allocation can complete database-only deletion; it has issued no runtime
effects. Unledgered pre-cutover rows have no such provenance. Resume preserves the same UID/generation.
There is no automatic same-Session Actor replacement, old-session migration,
legacy enrollment or Full checkpoint forks. Same-Actor resume supports both DATA
and FULL state. Quiescence verifies the snapshot scope configured by the pinned
ActorTemplate; checkpoint creation retains that exact scope. An uncertain original issuance is deliberately unavailable
until separately reviewed reconciliation; source preparation does not add an
operator recovery/adoption API.

## Callback transaction and policy boundary

The owning service observes Substrate before any database transaction. The store
then locks Session followed by generation, verifies the current active binding,
and runs database-only callback operations in that same transaction. Nested task
operations use savepoints and cannot escape to a pool. Revocation takes the same
lock order. No external call occurs under these locks. A request already admitted
can finish concurrently with external Substrate deletion; this is not atomic
cross-system deletion or process-activation attestation. Every later request
performs another fresh fail-closed UID/state read.

User egress credentials cannot select the reserved header or `kagent-runtime-`
Secret reference class, including URL-encoded references. The trusted policy
builder reserves the callback hostname exclusively for runtime injection and
refuses alternate ports/schemes or any user credentials on it. Its only injection
rule uses the controller's exact existing callback origin. Both native Claude
and Codex already use the shared Go TaskStore adapter for boot GetWorkspace and
subsequent callbacks, so the adapter supplies the mandatory placeholder to both.

## Installation and qualification

Ephemeral runtime injection Secrets use the controller resource namespace and
labels `kagent.dev/runtime-injection=true` and
`kagent.dev/runtime-generation=<generation UUID>`. The chart adds a namespaced
get/create/delete Secret Role for the controller. Kubernetes RBAC cannot express
label-based Secret authorization; existing broader grants are not narrowed here.
Installation must separately constrain credential-provider namespace/labels,
remove overlapping grants and exclude actor access to provider/mint/Kubernetes
APIs. Ephemeral Secrets must remain outside GitOps managed inventory. No live
Secret creation, image publication, pins or cluster qualification is performed
by this source change.

The callback remains the existing HTTP controller origin on port 8083. This
slice does not separate listeners or claim an encrypted gateway-to-controller
hop; it retains the trusted-cluster transport assumption pending listener/TLS
cutover. The controller must own this hostname and expose no credential reflection
or arbitrary forwarding receiver. Reserved-origin source checks and fakes do not
prove deployed isolation, cache/tunnel behavior or controller/network exclusivity.
The old Python TaskStore adapter is not migrated by this native-harness slice and
cannot use unsigned authentication. It needs a separately scoped consumer update
before it can operate against this admission contract. The optional unsigned
Python callback probe is removed from the Go gRPC fixture accordingly.
