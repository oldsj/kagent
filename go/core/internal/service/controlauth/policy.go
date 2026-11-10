// Package controlauth owns the operator-configured Session creation policy:
// which Agents API callers may create Sessions for, and which Secrets they may
// bind as Session credentials.
package controlauth

import (
	"context"
	"errors"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
)

// ErrDenied is returned for every refusal. It names no rule, so a caller
// cannot probe the allowlist.
var ErrDenied = errors.New("session creation is not authorized by the session policy")

const (
	purposeGitRead = "git-read"
	purposeGitPush = "git-push"
	purposeMCP     = "mcp"

	gitReadSecretPattern = "mainloop-git-read-*"
	gitPushSecretPattern = "mainloop-git-push-*"
)

type credentialRule struct {
	secretNamePattern, key, origin, header string
}

// Policy is immutable after New.
type Policy struct {
	namespace string
	agents    []string
	rules     []credentialRule
}

// New validates the configured policy. A Git rule must be exactly the trusted
// proxy tuple for its purpose, so a typo cannot widen which Secrets reach the
// proxies; an mcp rule may not target a Git proxy at all.
func New(config *configv1.SessionPolicy) (*Policy, error) {
	if len(validation.IsDNS1123Label(config.GetNamespace())) != 0 || len(config.GetAgents()) == 0 {
		return nil, errors.New("session policy requires a namespace and an Agent allowlist")
	}
	for _, name := range config.GetAgents() {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return nil, errors.New("session policy has an invalid Agent name")
		}
	}
	cleartext := config.GetCleartextMcpOrigins()
	for i, origin := range cleartext {
		if !validCleartextMCPOrigin(origin) || reservedGitOrigin(origin) {
			return nil, errors.New("session policy cleartext MCP origins must be exact http origins on svc.cluster.local hosts")
		}
		if slices.Contains(cleartext[:i], origin) {
			return nil, errors.New("session policy has a duplicate cleartext MCP origin")
		}
	}
	policy := &Policy{namespace: config.GetNamespace(), agents: slices.Clone(config.GetAgents())}
	for _, rule := range config.GetCredentials() {
		origin, err := url.Parse(rule.GetOrigin())
		_, patternErr := path.Match(rule.GetSecretNamePattern(), "probe")
		if rule.GetSecretNamePattern() == "" || patternErr != nil || rule.GetKey() == "" || rule.GetHeader() != "authorization" ||
			err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Path != "" {
			return nil, errors.New("session policy has an invalid credential rule")
		}
		switch rule.GetPurpose() {
		case purposeGitRead:
			if rule.GetOrigin() != workspace.ReadProxyOrigin || rule.GetSecretNamePattern() != gitReadSecretPattern || rule.GetKey() != "authorization" {
				return nil, errors.New("session policy git-read rule must be the trusted read proxy tuple")
			}
		case purposeGitPush:
			if rule.GetOrigin() != workspace.PushProxyOrigin || rule.GetSecretNamePattern() != gitPushSecretPattern || rule.GetKey() != "authorization" {
				return nil, errors.New("session policy git-push rule must be the trusted push proxy tuple")
			}
		case purposeMCP:
			if reservedGitOrigin(rule.GetOrigin()) || (origin.Scheme != "https" && (origin.Scheme != "http" || !slices.Contains(cleartext, rule.GetOrigin()))) {
				return nil, errors.New("session policy mcp rule must use https or a listed cleartext MCP origin, never a Git proxy")
			}
		default:
			return nil, errors.New("session policy has an invalid credential purpose")
		}
		policy.rules = append(policy.rules, credentialRule{
			secretNamePattern: rule.GetSecretNamePattern(), key: rule.GetKey(), origin: rule.GetOrigin(), header: rule.GetHeader(),
		})
	}
	return policy, nil
}

func reservedGitOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return workspace.GitProxyHost(workspace.NormalizeHost(u.Hostname()))
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

// internal reports controller-originated work, which is not an API caller.
func internal(ctx context.Context) bool {
	session, _ := auth.AuthSessionFrom(ctx)
	_, ok := session.(auth.ControlPlaneSession)
	return ok
}

// CheckCreateSession admits a Session only for an allowlisted Agent and only
// with credentials that each match one rule exactly. Two credentials for the
// same origin and header are refused even when both match.
func (p *Policy) CheckCreateSession(ctx context.Context, agent *apiv1.ResourceReference, credentials []*apiv1.SessionCredential) error {
	if internal(ctx) {
		return nil
	}
	if agent.GetNamespace() != p.namespace || !slices.Contains(p.agents, agent.GetName()) {
		return ErrDenied
	}
	seen := make(map[string]bool, len(credentials))
	for _, credential := range credentials {
		header := strings.ToLower(credential.GetHeader())
		allowed := slices.ContainsFunc(p.rules, func(rule credentialRule) bool {
			match, _ := path.Match(rule.secretNamePattern, credential.GetSecretRef().GetName())
			return match && rule.key == credential.GetSecretRef().GetKey() && rule.origin == credential.GetOrigin() && rule.header == header
		})
		key := credential.GetOrigin() + "\x00" + header
		if !allowed || seen[key] {
			return ErrDenied
		}
		seen[key] = true
	}
	return nil
}
