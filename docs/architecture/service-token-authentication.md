# Mainloop service-token authentication

`controller.auth.mode: service-token` enables a fixed Mainloop service identity
and a default-deny control policy. The default remains `insecure`; trusted-proxy
behavior is unchanged. This credential is for the Mainloop backend, never a
browser or actor. TLS, network isolation, Secret publisher policy, and verified
runtime identity must be supplied separately.

Configure `controller.auth.serviceToken.secretName` with an existing Kubernetes
Secret. `currentKey` defaults to `current`; `nextKey` is empty by default. Each
configured key must contain an RFC6750 bearer token of at least 32 bytes.
The file limit is 4096 bytes, including an optional trailing newline. The full Secret volume is mounted read-only at
`/run/kagent-control-auth`, without subPath. Tokens are not Helm values and are
never placed in authenticated Session state, logs, or the database.

Files are bounded and reread for every authentication attempt. Missing, malformed,
or unreadable configured files fail closed at startup and on reload. To rotate,
configure a valid next key, switch Mainloop to it, and replace current with next.
Then remove the overlap configuration/key together. Do not configure an empty
next key as a placeholder. Revocation takes effect after the mounted Secret has
updated; existing authenticated streams retain their admission decision. Cut
streams at the trusted frontend if immediate stream revocation is required.

Operator policy is serialized as `KAGENT_AUTH_SERVICE_POLICY`. The remaining
settings are `KAGENT_AUTH_SERVICE_TOKEN_CURRENT_FILE` and
`KAGENT_AUTH_SERVICE_TOKEN_NEXT_FILE`. Policy requires an explicit namespace and
Agent list. Helm defaults to namespace `kagent` and these five Agents:
`mainloop-main`, `claude-subscription`, `codex-subscription-https`,
`claude-workspace`, and `codex-workspace`. Set the namespace explicitly when the
installation uses another namespace.

Allowed RPCs are Session Create/Get/List/Suspend/Resume/Delete, Agent GetAgent,
and A2A SendStreamingMessage/GetTask/ListTasks/CancelTask/SubscribeToTask/
GetExtendedAgentCard. Sessions must be created by `mainloop` and reference an
allowed Agent. Lists apply that intersection before page slicing and cursor
construction. All-creators lists and share tokens are denied. A2A requires an
allowed Agent route; task IDs resolve through their stored Session and streaming
sends require an existing owned context or task. Synchronous SendMessage, MCP,
reflection, and all other control methods are denied. Reflection registration is
suppressed in service-token mode even when `controller.grpc.reflection` or
`KAGENT_GRPC_REFLECTION` is enabled. Public RPC exceptions in this mode are
limited to SystemService GetVersion and health Check/List/Watch; other methods
classified as public still deny. Insecure and trusted-proxy reflection settings
continue to work unchanged. Internal controller calls
use the separate ControlPlaneSession authority. Runtime TaskStore authentication
continues to use its existing authenticator in this slice.

Credential references are denied unless they match a configured approved tuple:

```yaml
controller:
  auth:
    mode: service-token
    serviceToken:
      secretName: mainloop-control-bearer
      credentials:
        - namespace: kagent
          secretNamePattern: mainloop-mcp-binding-*
          key: authorization
          origin: https://mainloop.example.com
          header: authorization
          purpose: mcp
```

The tuple uses the Agent namespace, a shell-style Secret name pattern, exact key
and HTTPS origin, and a case-insensitive authorization header. Only MCP-purpose
credentials are supported. Duplicate origin/header pairs and unapproved extra
headers deny before Session reservation or Secret resolution. Existing checkout
validation against the prepared Agent revision still applies.

The name pattern is an operator trust boundary for the Mainloop MCP publisher;
it does not establish which Mainloop binding owns a matching Secret. Use exact
Secret names when that distinction matters. A broader pattern permits every
matching published binding; trusted per-binding publisher records are required
to enforce stronger association. Purpose is operator policy, not a caller claim
or a Secret label lookup. No label alone confers authority.

Control bearers are stripped from outbound HTTP headers and A2A service metadata.
Runtime identity headers continue to follow the existing callback contract; this
slice does not claim verified runtime identity or listener separation.
