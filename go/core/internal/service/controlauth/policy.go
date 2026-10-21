// Package controlauth owns the operator-configured Mainloop service policy.
package controlauth

import (
	"context"
	"errors"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"buf.build/go/protovalidate"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/kagent-dev/kagent/go/api/authorization"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"k8s.io/apimachinery/pkg/util/validation"
)

var errDenied = errors.New("service operation is not authorized")

type CredentialRule struct {
	Namespace         string `json:"namespace"`
	SecretNamePattern string `json:"secretNamePattern"`
	Key               string `json:"key"`
	Origin            string `json:"origin"`
	Header            string `json:"header"`
	Purpose           string `json:"purpose"`
}

type Config struct {
	Namespace                        string           `json:"namespace"`
	Agents                           []string         `json:"agents"`
	Credentials                      []CredentialRule `json:"credentials"`
	CleartextMCPOrigins              []string         `json:"cleartextMCPOrigins"`
	DevelopmentEnvironmentRegistries []string         `json:"developmentEnvironmentRegistries"`
	// GitInstanceNamespaces lists the Mainloop Git instances (by the namespace
	// their git-read/git-push proxies and Secrets live in) this controller may
	// approve Git credential tuples for. A nil or empty value defaults to
	// ["mainloop"], the original single-instance behavior.
	GitInstanceNamespaces []string `json:"gitInstanceNamespaces,omitempty"`
}

type Policy struct {
	config           Config
	runtimePlatforms map[string]bool
}

var _ auth.CollectionAuthorizer = (*Policy)(nil)

func New(config Config) (*Policy, error) {
	if len(validation.IsDNS1123Label(config.Namespace)) != 0 || len(config.Agents) == 0 {
		return nil, errors.New("service policy requires a namespace and Agent allowlist")
	}
	for _, name := range config.Agents {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return nil, errors.New("invalid service Agent allowlist")
		}
	}
	gitInstanceNamespaces := config.GitInstanceNamespaces
	if len(gitInstanceNamespaces) == 0 {
		gitInstanceNamespaces = []string{"mainloop"}
	}
	seenNamespace := make(map[string]bool, len(gitInstanceNamespaces))
	for _, namespace := range gitInstanceNamespaces {
		if len(validation.IsDNS1123Label(namespace)) != 0 {
			return nil, errors.New("invalid Git instance namespace allowlist")
		}
		if seenNamespace[namespace] {
			return nil, errors.New("Git instance namespace allowlist contains a duplicate")
		}
		seenNamespace[namespace] = true
	}
	for i, namespace := range gitInstanceNamespaces {
		for _, other := range gitInstanceNamespaces[i+1:] {
			if overlappingGitInstanceNamespaces(namespace, other) {
				return nil, errors.New("Git instance namespace allowlist entries produce overlapping Secret name patterns")
			}
		}
	}
	config.GitInstanceNamespaces = append([]string(nil), gitInstanceNamespaces...)
	for i, origin := range config.CleartextMCPOrigins {
		if !validCleartextMCPOrigin(origin) || reservedGitOrigin(origin, config.GitInstanceNamespaces) {
			return nil, errors.New("cleartext MCP allowlist requires exact HTTP origins on svc.cluster.local hosts")
		}
		if slices.Contains(config.CleartextMCPOrigins[:i], origin) {
			return nil, errors.New("cleartext MCP allowlist contains a duplicate origin")
		}
	}
	for _, rule := range config.Credentials {
		origin, err := url.Parse(rule.Origin)
		_, patternErr := path.Match(rule.SecretNamePattern, "probe")
		if rule.Namespace != config.Namespace || rule.SecretNamePattern == "" || patternErr != nil || rule.Key == "" || rule.Header != "authorization" || err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Path != "" {
			return nil, errors.New("invalid approved credential tuple")
		}
		switch rule.Purpose {
		case "git-read":
			if !approvedGitInstanceTuple(config.GitInstanceNamespaces, rule, true) {
				return nil, errors.New("invalid approved Git read tuple")
			}
		case "git-push":
			if !approvedGitInstanceTuple(config.GitInstanceNamespaces, rule, false) {
				return nil, errors.New("invalid approved Git push tuple")
			}
		case "mcp":
			if reservedGitOrigin(rule.Origin, config.GitInstanceNamespaces) || (origin.Scheme != "https" && (origin.Scheme != "http" || !slices.Contains(config.CleartextMCPOrigins, rule.Origin))) {
				return nil, errors.New("invalid approved MCP tuple")
			}
		default:
			return nil, errors.New("invalid credential purpose")
		}
	}
	config.Agents = append([]string(nil), config.Agents...)
	config.Credentials = append([]CredentialRule(nil), config.Credentials...)
	config.CleartextMCPOrigins = append([]string(nil), config.CleartextMCPOrigins...)
	for _, registry := range config.DevelopmentEnvironmentRegistries {
		parsed, err := url.Parse("https://" + registry)
		if err != nil || registry == "" || parsed.Host != registry || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || registry != strings.ToLower(registry) {
			return nil, errors.New("development environment allowlist requires registry authorities without paths")
		}
	}
	config.DevelopmentEnvironmentRegistries = append([]string(nil), config.DevelopmentEnvironmentRegistries...)
	return &Policy{config: config}, nil
}

func reservedGitOrigin(origin string, gitInstanceNamespaces []string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := workspace.NormalizeHost(u.Hostname())
	for _, namespace := range gitInstanceNamespaces {
		readOrigin, pushOrigin := workspace.GitInstanceOrigins(namespace)
		readHost, _ := url.Parse(readOrigin)
		pushHost, _ := url.Parse(pushOrigin)
		if host == readHost.Hostname() || host == pushHost.Hostname() {
			return true
		}
	}
	return false
}

// overlappingGitInstanceNamespaces reports whether two Git instance namespaces
// would produce Secret name glob patterns where one could match a Secret
// intended for the other, for either the read or push purpose.
func overlappingGitInstanceNamespaces(a, b string) bool {
	for _, suffix := range []string{"-git-read-", "-git-push-"} {
		if strings.HasPrefix(b, a+suffix) || strings.HasPrefix(a, b+suffix) {
			return true
		}
	}
	return false
}

// approvedGitInstanceTuple reports whether rule exactly matches one of the
// allowlisted Git instances' read (or push) origin and Secret name pattern.
func approvedGitInstanceTuple(gitInstanceNamespaces []string, rule CredentialRule, read bool) bool {
	if rule.Key != "authorization" {
		return false
	}
	for _, namespace := range gitInstanceNamespaces {
		readOrigin, pushOrigin := workspace.GitInstanceOrigins(namespace)
		readPattern, pushPattern := workspace.GitInstanceSecretPatterns(namespace)
		if read && rule.Origin == readOrigin && rule.SecretNamePattern == readPattern {
			return true
		}
		if !read && rule.Origin == pushOrigin && rule.SecretNamePattern == pushPattern {
			return true
		}
	}
	return false
}

// validCleartextMCPOrigin accepts only canonical, exact in-cluster HTTP origins.
func validCleartextMCPOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	host := parsed.Hostname()
	if !strings.HasSuffix(host, ".svc.cluster.local") || len(validation.IsDNS1123Subdomain(host)) != 0 {
		return false
	}
	authority := host
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
		authority += ":" + port
	}
	return origin == "http://"+authority
}

// WithRuntimePlatforms binds selection to the operator's current payload catalog.
// It returns a copy, keeping the authenticator's original policy immutable.
func (p *Policy) WithRuntimePlatforms(platforms []string) *Policy {
	result := &Policy{config: p.config, runtimePlatforms: make(map[string]bool)}
	for _, platform := range platforms {
		result.runtimePlatforms[platform] = true
	}
	return result
}

func (p *Policy) approvedDevelopmentImage(image string) bool {
	selection := &api.DevelopmentEnvironment{Image: image, Platform: "linux/amd64", PolicyIdentity: "validation"}
	if protovalidate.Validate(selection) != nil {
		return false
	}
	registry, _, qualified := strings.Cut(image, "/")
	if !qualified {
		return false
	}
	return slices.Contains(p.config.DevelopmentEnvironmentRegistries, registry)
}

// CheckDevelopmentEnvironment validates all caller selection fields before any
// store/preparer effect. Only a verified service principal can select D; internal
// controller sessions and spoofable proxy/insecure user identities cannot.
func (p *Policy) CheckDevelopmentEnvironment(ctx context.Context, agent *api.ResourceReference, selection *api.DevelopmentEnvironment) error {
	session, ok := auth.AuthSessionFrom(ctx)
	if !ok || selection == nil || protovalidate.Validate(selection) != nil || strings.TrimSpace(selection.GetPolicyIdentity()) == "" || !p.agent(agent.GetNamespace(), agent.GetName()) || !p.runtimePlatforms[selection.GetPlatform()] {
		return errDenied
	}
	return p.Check(ctx, session.Principal(), auth.VerbCreate, auth.Resource{Type: "DevelopmentEnvironment", Namespace: agent.GetNamespace(), Name: selection.GetImage()})
}

func (p *Policy) agent(namespace, name string) bool {
	if namespace != p.config.Namespace {
		return false
	}
	return slices.Contains(p.config.Agents, name)
}

func internal(ctx context.Context) bool {
	session, _ := auth.AuthSessionFrom(ctx)
	_, ok := session.(auth.ControlPlaneSession)
	return ok
}

func (p *Policy) Check(ctx context.Context, principal auth.Principal, verb auth.Verb, resource auth.Resource) error {
	if resource.Type == "DevelopmentEnvironment" {
		if principal.Service == auth.MainloopService && principal.User.ID == auth.MainloopService && principal.Agent.ID == "" && verb == auth.VerbCreate && resource.Namespace == p.config.Namespace && p.approvedDevelopmentImage(resource.Name) {
			return nil
		}
		return errDenied
	}
	if internal(ctx) {
		return nil
	}
	if principal.Service != auth.MainloopService || principal.User.ID != auth.MainloopService || principal.Agent.ID != "" {
		return errDenied
	}
	switch resource.Type {
	case "Agent":
		if verb == auth.VerbGet && p.agent(resource.Namespace, resource.Name) {
			return nil
		}
	case "Session":
		if !strings.Contains(resource.Name, "/") && (verb == auth.VerbGet || verb == auth.VerbCreate || verb == auth.VerbUpdate || verb == auth.VerbDelete) {
			return nil
		}
	case "Checkpoint":
		// Reads and deletes only. The store scopes every checkpoint to the
		// principal that took it, so this identity reaches only its own. Renaming
		// stays denied: the service has no use for it.
		if !strings.Contains(resource.Name, "/") && (verb == auth.VerbGet || verb == auth.VerbDelete) {
			return nil
		}
	}
	return errDenied
}

func (p *Policy) Scope(ctx context.Context, principal auth.Principal, verb auth.Verb, resourceType string) (authorization.AuthorizationScope, error) {
	if internal(ctx) {
		return authorization.AuthorizationScope{Kind: authorization.ScopeAll}, nil
	}
	return authorization.AuthorizationScope{Kind: authorization.ScopeNone}, nil
}

func (p *Policy) CheckSession(ctx context.Context, session *api.Session) error {
	if internal(ctx) {
		return nil
	}
	if session.GetCreator() != auth.MainloopService || !p.agent(session.GetAgent().GetNamespace(), session.GetAgent().GetName()) {
		return errDenied
	}
	return nil
}

func (p *Policy) CheckAgent(ctx context.Context, agent *api.ResourceReference) error {
	if internal(ctx) {
		return nil
	}
	if !p.agent(agent.GetNamespace(), agent.GetName()) {
		return errDenied
	}
	return nil
}

func (p *Policy) CheckCreateSession(ctx context.Context, agent *api.ResourceReference, credentials []*api.SessionCredential) error {
	if internal(ctx) {
		return nil
	}
	if err := p.CheckAgent(ctx, agent); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, credential := range credentials {
		allowed := false
		for _, rule := range p.config.Credentials {
			match, _ := path.Match(rule.SecretNamePattern, credential.GetSecretRef().GetName())
			if match && rule.Namespace == agent.GetNamespace() && rule.Key == credential.GetSecretRef().GetKey() && rule.Origin == credential.GetOrigin() && rule.Header == strings.ToLower(credential.GetHeader()) {
				allowed = true
				break
			}
		}
		key := credential.GetOrigin() + "\x00" + strings.ToLower(credential.GetHeader())
		if !allowed || seen[key] {
			return errDenied
		}
		seen[key] = true
	}
	return nil
}

// CheckMethod is used before decoding/dispatch on both gRPC transports.
func CheckMethod(method string) error {
	switch method {
	case api.SessionService_PrepareSessionWorkspace_FullMethodName, api.SessionService_CreateSession_FullMethodName, api.SessionService_GetSession_FullMethodName, api.SessionService_ListSessions_FullMethodName, api.SessionService_SuspendSession_FullMethodName, api.SessionService_ResumeSession_FullMethodName, api.SessionService_DeleteSession_FullMethodName, api.AgentService_GetAgent_FullMethodName,
		api.CheckpointService_CreateCheckpoint_FullMethodName, api.CheckpointService_GetCheckpoint_FullMethodName, api.CheckpointService_ListCheckpoints_FullMethodName, api.CheckpointService_DeleteCheckpoint_FullMethodName, api.CheckpointService_ForkSession_FullMethodName,
		a2apb.A2AService_SendStreamingMessage_FullMethodName, a2apb.A2AService_GetTask_FullMethodName, a2apb.A2AService_ListTasks_FullMethodName, a2apb.A2AService_CancelTask_FullMethodName, a2apb.A2AService_SubscribeToTask_FullMethodName, a2apb.A2AService_GetExtendedAgentCard_FullMethodName:
		return nil
	default:
		return errDenied
	}
}

func CheckA2AMethod(method string) error {
	return CheckMethod("/" + a2apb.A2AService_ServiceDesc.ServiceName + "/" + method)
}
