# Runtime credential injection

Kagent requires Substrate **v0.4.0-alpha1**. The compiler turns ModelConfig API
keys and Secret-backed RemoteMCPServer headers into destination-scoped egress
bindings. Substrate's gateway fetches the referenced Kubernetes Secret and
replaces the outgoing HTTP header when the request carries a placeholder. SDKs
receive an inert placeholder where they require an API key; real credentials never enter compiled environments,
runtime configuration, or revision provenance.

Bindings are persisted with the prepared revision and installed before a
session becomes ready, including retries and checkpoint forks. Secret names,
keys, destinations, and headers affect revision identity. Secret values and
Secret UIDs do not. Rotation is handled by the gateway; its credential cache can
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
kagent chart. Empty grants deny all credential fetches. Kagent compiles
same-namespace references such as
`ate-secret://k8s.io/default/kagent/model-auth/api-key`.

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
match its scheme, DNS name, and port; credential bindings remain scoped to the
hostname and header. Different credentials for the same hostname and header
are rejected, including conflicts between models, memory embeddings, and MCP
servers. Use distinct DNS names for
origins requiring different credentials. IP-address destinations are unsupported
by Substrate egress policies.

A Git credential binds to its single origin's hostname and the `authorization`
header, so the one-credential-per-host-and-header rule applies to it like any
other source. The gateway overwrites an `Authorization` header only when the
client already sent one. The Git bootstrap therefore writes a placeholder
`http.https://<host>/.extraHeader` into the workspace repository's own config,
never the global one; the real value exists only at the gateway. See
[Git workspace bootstrap](runtime-and-lifecycle.md#git-workspace-bootstrap).

Harness and SandboxTemplate environment entries accept only literal `value`
strings, including empty strings. Configure Secret-backed credentials on
ModelConfig or RemoteMCPServer for gateway injection.

AWS IAM signing keys, Google service-account keys, and OAuth client credentials
require mechanisms beyond static header injection and are rejected rather than
serialized into runtimes.
Caller-token passthrough retains its existing behavior. A passthrough model
cannot share a hostname with static gateway credentials, which would override
the caller's authentication.

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
`CODEX_HOME` and file credential storage. After interactive login, load its full
`auth.json` into the control-side Secret key `auth.json` and its derived access
token into the ModelConfig's existing `apiKeySecretKey` (normally
`access-token`), in one version-checked update. Do not use client-side apply,
which retains the credential-bearing manifest in an annotation. Remove the
temporary home after seeding. Neither the gateway nor an Actor receives the
`auth.json` key; bindings continue to reference only the access-token key.

Set controller environment variable `KAGENT_CHATGPT_REFRESH_IMAGE` to an
immutable digest of the Codex Harness image containing
`/usr/local/bin/kagent-credential-refresh` and official Codex **0.148.0**.
The worker checks the executable version before requesting refresh. An empty
image setting disables automatic refresh. Leader election must be enabled.
Secrets without an `auth.json` key retain manual access-token rotation.

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
Paths, user information, queries, and fragments are rejected. Injection is
host-wide: scheme and port constrain egress admission, but credentials apply to
all allowed origins on the same hostname. Use a dedicated MCP origin serving
only its intended endpoints. Header names are case-insensitive. Duplicate
host/header bindings and collisions with revision credentials are rejected.
References persist in `Session.credentials`; retrying a request ID with changed
references conflicts. Policy construction canonicalizes the merged list, so
retries produce the same policy. Resume retains it; checkpoint forks inherit no
Session credentials.

Credential-bearing `CreateSession` is a trusted control-plane capability. A
caller may reference any valid Secret key in the Agent namespace; same-namespace
validation does not establish that the key belongs to that caller. Isolate these
callers and authorize credential selection in the owning control plane before
forwarding requests to kagent.

Agentgateway supports cleartext HTTP injection when its HTTP route's
`substrateEgress` policy has a credential provider. The binding's exact hostname
and the Session egress policy limit where it applies; an unbound hostname keeps
the placeholder. The provider must also grant the actor's atespace access to
the Agent namespace. The Envoy dataplane skips cleartext credential injection
in stock Substrate v0.4.0-alpha1. Restrict the destination listener with NetworkPolicy.
Secret lookup failures surface at the gateway; creation does not read or verify keys.
