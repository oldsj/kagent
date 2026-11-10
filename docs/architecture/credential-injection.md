# Runtime credential injection

Kagent requires Substrate **v1.0.0**. The runtime compiler turns
ModelConfig API keys and Secret-backed RemoteMCPServer headers into pinned
destination-scoped bindings. Independently, EgressPolicy admission supplies
additional live bindings. Substrate's gateway fetches the referenced Kubernetes Secret and
replaces the outgoing HTTP header when the request carries a placeholder. SDKs
receive an inert placeholder where they require an API key; real credentials never enter compiled environments,
runtime configuration, or revision provenance.

Inferred integration bindings are persisted with the prepared revision; their
Secret names, keys, destinations, and headers affect revision identity. Explicit
EgressPolicy bindings are resolved from current policies and update live actors
without changing that revision. Conflicts hold new grants and report the
independent [EgressPoliciesApplied condition](egress-policies.md#preparation-and-lifecycle);
they do not block runtime compilation. Both are installed before execution, including
creation retries, resume, and checkpoint forks. Secret values and Secret UIDs
do not affect revision identity. Rotation is handled by the gateway; its credential cache can
take up to five minutes to refresh, without recompiling or restarting an agent.

## Installation

The Substrate chart installs the credential provider and HTTPS interception gateway.
Grant each agent atespace access to its credential namespace in the Substrate
release values:

```yaml
credentialProvider:
  namespacePolicies:
    - atespace: kagent
      allowedNamespaces: [kagent]
```

For an embedded Substrate chart, put these values under `substrate:` in the
kagent chart. Empty grants deny all credential fetches. Kagent compiles namespace-qualified references such as
`ate-secret://k8s.io/default/kagent/model-auth/api-key`.
For [shared models and MCP servers](cross-namespace-references.md), grant the
consuming actor atespace access to the target Secret namespace explicitly.
Resource reference permission does not change credential-provider grants.

Create the gateway CA in the Substrate release namespace before waiting for
the rollout (alongside the other Substrate CA pools):

```sh
kubectl-ate admin make-ca-pool --ca-id=1 --name=egress-mitm-ca-pool \
  --secret-namespace=ate-system --key-type=ECDSAP256
```

The local setup script and CI install both the grant and CA. ActorTemplates
project `egress-mitm.ate.dev` into `/run/kagent/egress/trust-bundle.pem` and set
the Go/OpenSSL, Node, Python requests, AWS, curl, and git CA environment variables.
Custom BYO clients must honor the configured trust bundle. Harness environment
overrides of these variables are rejected.

## Supported credentials

| Source | Header |
| --- | --- |
| OpenAI API key | `authorization: Bearer <key>` |
| Anthropic API key | `x-api-key: <key>` |
| Claude Code OAuth token | `authorization: Bearer <token>` |
| Codex ChatGPT access token | `authorization: Bearer <token>` |
| Azure OpenAI and Foundry OpenAI API key | `api-key: <key>` |
| Foundry Anthropic API key | `x-api-key: <key>` |
| Gemini API key | `x-goog-api-key: <key>` |
| Bedrock bearer token | `authorization: Bearer <token>` |
| RemoteMCPServer Secret-backed header | Configured header; Secret contains its full value |
| Git origin (`spec.git.credentialSecretRef` on a Harness) | `authorization: <value>`; Secret contains the full header value, for example `Basic <base64 of x-access-token:TOKEN>` |

Provider endpoint overrides determine the allowed HTTP(S) origin. Egress rules
and credential bindings both match its scheme, DNS name, and port. Different
credentials for the same origin and header are rejected, including conflicts
between models, memory embeddings, MCP servers, and explicit policies. Distinct
ports on the same DNS name can use different credentials. IP-address destinations
are unsupported by Substrate egress policies.

The v1alpha3 revision format stores full origins. It assumes a clean installation;
previously persisted hostname-only credential bindings are rejected rather than
reinterpreted across every port on that host.

A Git credential binds to its single origin's hostname and the `authorization`
header, so the one-credential-per-host-and-header rule applies to it like any
other source. The gateway overwrites an `Authorization` header only when the
client already sent one. The Git bootstrap therefore writes a placeholder
`http.https://<host>/.extraHeader` into the workspace repository's own config,
never the global one; the real value exists only at the gateway. See
[Git workspace bootstrap](runtime-and-lifecycle.md#git-workspace-bootstrap).

Harness and SandboxTemplate environment entries accept only literal `value`
strings, including empty strings. Configure Secret-backed credentials on
ModelConfig, RemoteMCPServer, or [EgressPolicy](egress-policies.md) for gateway injection.

AWS IAM signing keys, Google service-account keys, and OAuth client credentials
require mechanisms beyond static header injection and are rejected rather than
serialized into runtimes.

Caller-token passthrough is set with `tokenPassthrough` on ModelConfig or
RemoteMCPServer. It forwards the bearer token from the incoming A2A request
instead of a gateway credential. A passthrough model or MCP server cannot share
an origin with static gateway credentials, which would override the caller's
authentication. A passthrough RemoteMCPServer cannot set `Authorization` in
`headersFrom`.

Compilation records each passthrough client as a target {origin, header,
prefix} in the revision. It rejects a client with no credential target (SAP AI
Core), a client that shares its origin with an MCP server or model that did not
opt in, and an explicit header that matches a target header.

RemoteMCPServer passthrough is implemented only by the Go kagent runtime.
Codex and Claude reject it at compilation, and the Python runtime fails at
startup. A request without a caller token fails instead of connecting
anonymously. The runtime discovers tools on first use, because startup has no
caller token. Controller discovery also has no caller token, so servers that
require one should set the `kagent.dev/discovery: disabled` label. The runtime
shares one MCP session per server. The standalone SSE stream and session
teardown therefore carry the token of the caller that opened the session.

### Gateway token passthrough

Without `app.Options.EgressPolicyCompilers`, runtimes forward the caller token
as described above. To keep it out of runtimes, an embedding program registers
compilers and its `Authenticator.UpstreamAuth` must replace the caller's
`Authorization` with `Bearer kagent-credential-injected`; registering compilers
alone changes nothing, and a forwarding authenticator still exposes the token.
The runtime forwards the placeholder as usual. Compilers run in order after the
built-in EgressPolicy compiler and may change anything it produced. Kagent then
validates the result, and every target must have a header. New create and resume
operations compile the policy before they are issued, so a failure stops them
before any lifecycle mutation. If a later compile or this validation fails, the
actor gets no egress until a retry succeeds. Kagent never names a credential provider.

This covers Go ADK MCP servers and models, OpenAI, Azure and Foundry memory
embeddings, and Python OpenAI and Anthropic models. Python MCP, Claude and
Codex still do not support passthrough.

## Actor JWT authentication

`ModelConfig.spec.actorJwt` and `RemoteMCPServer.spec.actorJwt` select the
executing Actor's identity independently of the invocation caller:

```yaml
actorJwt:
  audiences: [my-service]
  expirationSeconds: 900
```

Substrate mints, injects, and refreshes this token at the gateway. The runtime
only receives an inert SDK placeholder. Models use their provider's existing
credential header from the table above; MCP uses `Authorization: Bearer`.
The destination must trust Substrate's issuer and validate its own audience.
This option supports providers with configurable endpoints and header credentials.
Bedrock and Gemini use fixed AWS and Google endpoints, and Ollama's credential
path only targets its hosted service. These providers reject `actorJwt`; Kagent
does not exchange Actor JWTs for their credentials. Vertex and SAP OAuth
authentication are also unsupported.

The option is mutually exclusive with `tokenPassthrough` and model API-key
Secrets. Omit unused credential fields: `tokenPassthrough: false` still counts
as a set field for this union. MCP `headersFrom` may still supply other headers,
but cannot supply `Authorization`. Model `defaultHeaders` cannot override the
credential header.
Global MCP discovery is disabled automatically for Actor authentication;
the executing runtime discovers tools using its own identity. Control-plane
MCP calls cannot authenticate as an Actor.
These servers publish no global tool catalog, so their tools do not appear in
the UI tool picker; agents discover them at runtime.

EgressPolicy uses the same token settings as an alternative to `secretKeyRef`:

```yaml
spec:
  destinations:
    - origin: https://my-service.example.com
      authentication:
        header: Authorization
        prefix: "Bearer "
        actorJwt:
          audiences: [my-service]
          expirationSeconds: 900
```

EgressPolicy retains its explicit header and prefix. A client must send that
header for the gateway to replace it; arbitrary tools and BYO clients supply
their own placeholder. Managed model and MCP compilers set it automatically.

Audiences contain 1–16 unique, nonempty values. The `kagent-runtime` audience is
reserved for internal control-plane calls and cannot be exported. Lifetimes
range from 300 to 3600 seconds and default to 900 seconds. Actor JWT settings
follow the same grant lifetimes as Secret credentials: model and MCP bindings
are pinned in revisions, while EgressPolicy bindings update existing Actors.

## Deferred API fields

The source retains commented declarations for Harness and SandboxTemplate
`env[].credentialRef`, ModelConfig `openAI.tokenExchange`, and TLS
`caCertSecretRef`, `caCertSecretKey`, and `disableSystemCAs`. These fields are
absent from the served CRDs until their runtime paths are implemented. The
RemoteMCPServer TLS rotation `status.secretHash` is deferred with custom CAs.

ModelConfig and RemoteMCPServer TLS settings currently expose only
`disableVerify`, supported by the kagent compiler. Codex and Claude reject model
TLS settings and warn when ignoring RemoteMCPServer TLS settings. Runtime trust
for gateway injection is configured by the platform.

`apiKeySecret` remains available for supported gateway credentials. Secret-backed
AWS IAM signing, Vertex service-account, and SAP OAuth credentials still fail
compilation; sharing this field with supported providers does not enable those
credential modes.

For the Claude Harness, `anthropic.authMethod: oauthToken` reads the token from
the ModelConfig's `apiKeySecret` and `apiKeySecretKey`, sets only an inert
`CLAUDE_CODE_OAUTH_TOKEN` placeholder in the Actor, and binds the real token to
the Anthropic `Authorization: Bearer` request header at the egress gateway. The
default `apiKey` mode retains `ANTHROPIC_API_KEY` and `x-api-key` behavior.
`claude setup-token` creates a long-lived token without a refresh flow; rotate
it manually by replacing the referenced Secret value.

For the Codex Harness, `openAI.authMethod: chatGPT` uses `openAI.accountID` and
the current access token referenced by `apiKeySecret` / `apiKeySecretKey`.
The adapter writes synthetic JWTs, an empty refresh token, and a recent refresh
timestamp into private `CODEX_HOME/auth.json`; real bearer credentials remain
at the gateway, bound only to the backend host. That host is `chatgpt.com`
(`https://chatgpt.com/backend-api/codex`) unless `openAI.baseUrl` replaces the
backend, in which case egress and the credential binding follow the `baseUrl`
host. Actors cannot reach `auth.openai.com` or `api.openai.com`.
`responsesTransport: https` disables WebSockets; omission uses the native
WebSocket-capable provider and fallback.
The account ID is identifying data retained in the Actor and provenance.

## ChatGPT credential refresh

Create a **dedicated** ChatGPT login for the controller. Never seed it from a
developer's existing Codex home: rotating a shared refresh token invalidates
the other login. Use the official pinned Codex CLI with an isolated temporary
`CODEX_HOME` and file credential storage. Store the original full `auth.json`
and its exact paired access token in your secret store. A Secret operator can
publish this pair to a source Secret; kagent automatically bootstraps a separate
runtime Secret when its `kagent.dev/chatgpt-auth-seed-secret` annotation names
that source in the **same namespace**. ModelConfig references the runtime
Secret and its access-token key, with a nonempty `openAI.accountID` matching
both the file and the access token's `https://api.openai.com/auth` /
`chatgpt_account_id` claim.

For example, with an existing External Secrets Operator and SecretStore, keep
the dedicated login pair in two properties of one store entry. Replace the
store, entry, model and account placeholders with your deployment's values:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: chatgpt-auth-seed
  namespace: kagent
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: credential-store
    kind: SecretStore
  target:
    name: chatgpt-auth-seed
    creationPolicy: Owner
    template:
      type: Opaque
  data:
    - secretKey: auth.json
      remoteRef:
        key: dedicated-chatgpt-login
        property: auth.json
    - secretKey: access-token
      remoteRef:
        key: dedicated-chatgpt-login
        property: access-token
---
apiVersion: v1
kind: Secret
metadata:
  name: chatgpt-auth-runtime
  namespace: kagent
  annotations:
    kagent.dev/chatgpt-auth-seed-secret: chatgpt-auth-seed
type: Opaque
---
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata:
  name: chatgpt
  namespace: kagent
spec:
  provider: OpenAI
  model: <model-name>
  apiKeySecret: chatgpt-auth-runtime
  apiKeySecretKey: access-token
  openAI:
    authMethod: chatGPT
    accountID: <account-id-from-dedicated-login>
```

The operator owns only the source; the refresher owns the runtime credential
values. GitOps manages the runtime envelope and annotation, leaving runtime
data to kagent. Do not configure an operator or GitOps reconciliation to reset
the runtime data. No backend Secret copying or new RBAC is needed. Enable the
existing leader-owned credential controller with the refresh image below.

Each normal uncached sweep attempts bootstrap before refresh. It waits without
mutation when the source is absent or a nonterminal Secret-UID-owned Job exists.
Only a mutable Opaque runtime with no populated data and no refresh state,
claim, hash or retry annotations is eligible; empty unrelated keys are kept.
The source name must be a canonical Secret name, without a namespace prefix,
and cannot reference the runtime itself. All ChatGPT ModelConfigs sharing the
runtime must agree on the account and access key, which must differ from
`auth.json`. The seed must pass the deployed credential-file parser, contain a
nonempty refresh token and matching account identities, and have an access
token with more than ten minutes remaining. Invalid or ambiguous configuration
produces a sanitized controller failure without persisting a rejection or claim;
correcting the source or configuration permits a later sweep to bootstrap.

One resourceVersion-fenced update writes the original file bytes and paired
access token together, preserving the runtime UID and metadata. Bootstrap
neither invokes Codex nor spends refresh tokens. Conflicts and uncertain update
responses wait for the next sweep to reread the destination. Once populated,
the runtime automatically hands off to the existing refresher: seed updates,
controller restarts, expiry and rejection never reseed or overwrite it. A rejected
runtime requires the explicit dedicated re-authentication procedure below.

Without the annotation, the existing manual mode remains available: load the
full `auth.json` and its derived access token into the runtime in one
version-checked update. Do not use client-side apply for credentials, which
retains the credential-bearing manifest in an annotation. Remove the temporary
home after storing the dedicated login. Neither the gateway nor an Actor receives
the source or runtime `auth.json` key; bindings continue to reference only the
runtime access-token key, and Actors retain their synthetic auth file.

Set `config.credentialRefresh.chatgptImage` in the Helm values
(`credential_refresh.chatgpt_image` in the controller configuration) to an
immutable digest of the Codex Harness image, which contains
`/usr/local/bin/kagent-credential-refresh` and the Codex CLI version the Harness
pins. Setting it also renders the `credential-refresh` role: the controller's
other roles no longer carry the Job, ServiceAccount, Role, RoleBinding,
NetworkPolicy and Secret-update permissions the refresh needs.
The worker checks the executable version before requesting refresh. An empty
image setting disables automatic refresh. Leader election must be enabled.
Secrets without an `auth.json` key retain manual access-token rotation.

Refresh state annotations are operation markers, not an enrollment or health
status. A valid pair outside the ten-minute expiry margin is left unchanged,
and successful rotation removes its claim, state, hash, and retry annotations.
Replacing a populated runtime pair does not require an idle state annotation;
the controller schedules that pair from its new expiry without reading the seed.
Backend authentication failures do not currently trigger early refresh, so
rejection before the claimed expiry requires owner reauthentication.

The controller checks access-token expiry every 15 seconds and schedules
refresh ten minutes before expiry, including the gateway's five-minute cache
window. ModelConfigs sharing a Secret share one deterministic Job name and one
access-token key. Jobs use a private memory-backed Codex home, read-only root
filesystem, non-root user, no Pod restarts, no Job retries, and a three-minute
deadline. The Job ServiceAccount can only get/update that Secret through a
Role restricted by `resourceNames`. The controller requires create access to
Roles, RoleBindings, and NetworkPolicies in the watched credential namespaces.
These resources are owned by the Secret and contain no token values.

The Job invokes `codex app-server --strict-config --stdio`, initializes the
protocol, and sends only `account/read` with `refreshToken: true`. In pinned
Codex source, `codex-rs/app-server/src/request_processors/account_processor.rs`
calls `AuthManager::refresh_token` in
`codex-rs/login/src/auth/manager.rs`, which refreshes and persists the official
credential file. This path creates no thread or model turn. `codex login
status` only reads login state and does not perform this refresh. Kagent
implements no OAuth endpoint, grant, or client ID.

The controller claims a Secret with its resourceVersion before creating the
Job. Each Pod must acquire a second version-checked transition before starting
Codex, so duplicate Job execution cannot spend the same credential twice.
After Codex runs, the worker validates that both tokens changed, the account
matches, and the new access-token lifetime exceeds the safety margin. It
commits the full rotated file and derived access token in one Secret update.
Concurrent metadata edits are preserved by re-reading and retrying only
persistence; replacing the credential file or Secret UID cancels that write.
An uncertain successful update is recognized by comparing both stored values
in memory. Token values, upstream errors, stdout, and stderr are never logged.
Codex tracing is disabled and analytics/telemetry export is disabled.

Failures before the Pod acquires its claim retry after a durable jittered
one-to-two-minute delay. Once Codex may have used the token, failures and lost
Jobs require re-authentication instead of replaying it. Pinned `account/read`
suppresses the refresh error category, so even its successful response must
be checked against the rotated file; unchanged credentials are a failure.
This conservatively handles terminal rejection, timeout, and ambiguous
upstream failures. `ModelConfig` reports `ResolvedRefs=False` with reason
`ReauthenticationRequired`. Replacing the dedicated credential pair clears
the failure on reconciliation. Do not clear failure annotations to retry an
old credential. Loss of a Pod after rotation but before Secret persistence
also needs a new dedicated login; no cross-provider atomic commit exists.

The Job's NetworkPolicy permits DNS and TCP 443/6443 (including the login
authority and Kubernetes API, before or after service DNAT) and denies ingress. Standard Kubernetes
NetworkPolicy cannot restrict egress by DNS name; installations requiring
host-only egress must additionally configure their CNI's FQDN policy.
NetworkPolicy requires an enforcing CNI. Actor egress rules remain unchanged.

## Per-Session credentials

`CreateSessionRequest.credentials` accepts at most four `SessionCredential`
bindings, each containing `origin`, `header`, and `secret_ref { name, key }`.
The Secret belongs to the Agent's namespace and contains the complete header
value, including `Bearer ` for bearer authorization. Session bindings use no
additional prefix. The controller never reads the Secret value, and the runtime
receives only a placeholder header configured on its RemoteMCPServer.

The HTTP(S) origin must already be in the Session's pinned revision egress list.
Paths, user information, queries, and fragments are rejected. A binding applies
to that exact origin and header. Header names are case-insensitive. Duplicate
origin/header bindings and collisions with revision credentials are rejected.
References persist in `Session.credentials`; retrying a request ID with changed
references conflicts. Checkpoint forks inherit no Session credentials.

The controller applies them where it applies every Actor's egress policy: the
policy for a Session's Actor is the revision's effective access (pinned inputs
plus current EgressPolicy grants) with the Session's bindings added. It is
recomputed on each application, so a grant change keeps the Session's bindings.
If a later grant binds the same origin and header as a Session credential, the
merge fails and that Actor's policy update is reported as an error rather than
letting either credential silently win.

Credential-bearing `CreateSession` is a trusted control-plane capability. A
caller may reference any valid Secret key in the Agent namespace; same-namespace
validation does not establish that the key belongs to that caller. Isolate these
callers and authorize credential selection in the owning control plane before
forwarding requests to kagent.
