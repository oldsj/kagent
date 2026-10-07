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
	for i, origin := range config.CleartextMCPOrigins {
		if !validCleartextMCPOrigin(origin) {
			return nil, errors.New("cleartext MCP allowlist requires exact HTTP origins on svc.cluster.local hosts")
		}
		if slices.Contains(config.CleartextMCPOrigins[:i], origin) {
			return nil, errors.New("cleartext MCP allowlist contains a duplicate origin")
		}
	}
	for _, rule := range config.Credentials {
		origin, err := url.Parse(rule.Origin)
		_, patternErr := path.Match(rule.SecretNamePattern, "probe")
		if rule.Namespace != config.Namespace || rule.SecretNamePattern == "" || patternErr != nil || rule.Key == "" || rule.Purpose != "mcp" || rule.Header != "authorization" || err != nil || (origin.Scheme != "https" && !(origin.Scheme == "http" && slices.Contains(config.CleartextMCPOrigins, rule.Origin))) || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Path != "" {
			return nil, errors.New("invalid approved credential tuple")
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
	for _, allowed := range p.config.DevelopmentEnvironmentRegistries {
		if registry == allowed {
			return true
		}
	}
	return false
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
	for _, allowed := range p.config.Agents {
		if name == allowed {
			return true
		}
	}
	return false
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
	case api.SessionService_CreateSession_FullMethodName, api.SessionService_GetSession_FullMethodName, api.SessionService_ListSessions_FullMethodName, api.SessionService_SuspendSession_FullMethodName, api.SessionService_ResumeSession_FullMethodName, api.SessionService_DeleteSession_FullMethodName, api.AgentService_GetAgent_FullMethodName,
		a2apb.A2AService_SendStreamingMessage_FullMethodName, a2apb.A2AService_GetTask_FullMethodName, a2apb.A2AService_ListTasks_FullMethodName, a2apb.A2AService_CancelTask_FullMethodName, a2apb.A2AService_SubscribeToTask_FullMethodName, a2apb.A2AService_GetExtendedAgentCard_FullMethodName:
		return nil
	default:
		return errDenied
	}
}

func CheckA2AMethod(method string) error {
	return CheckMethod("/" + a2apb.A2AService_ServiceDesc.ServiceName + "/" + method)
}
