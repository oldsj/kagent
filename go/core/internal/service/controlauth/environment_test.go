package controlauth

import (
	"strings"
	"testing"

	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type environmentSession struct{ principal auth.Principal }

func (s environmentSession) Principal() auth.Principal { return s.principal }

func TestDevelopmentEnvironmentPolicy(t *testing.T) {
	policy, err := New(Config{Namespace: "kagent", Agents: []string{"writer"}, DevelopmentEnvironmentRegistries: []string{"registry.example:5000"}})
	require.NoError(t, err)
	original := policy
	policy = policy.WithRuntimePlatforms([]string{"linux/arm64"})
	agent := &api.ResourceReference{Namespace: "kagent", Name: "writer"}
	selection := &api.DevelopmentEnvironment{Image: "registry.example:5000/dev@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", PolicyIdentity: "registered-v1"}
	for _, name := range []string{"allowed", "wrong principal", "insecure identity", "agent identity", "off registry", "registry prefix", "tag", "unknown platform", "missing policy", "blank policy", "wrong agent", "wrong namespace", "empty allowlist", "no catalog", "internal"} {
		t.Run(name, func(t *testing.T) {
			selected := proto.CloneOf(selection)
			target := proto.CloneOf(agent)
			principal := auth.Principal{User: auth.User{ID: auth.MainloopService}, Service: auth.MainloopService}
			p := policy
			switch name {
			case "wrong principal":
				principal.User.ID = "other"
			case "insecure identity":
				principal.Service = ""
			case "agent identity":
				principal.Agent.ID = "actor"
			case "off registry":
				selected.Image = "evil.example/dev@sha256:" + strings.Repeat("a", 64)
			case "registry prefix":
				selected.Image = "registry.example:5000.evil/dev@sha256:" + strings.Repeat("a", 64)
			case "tag":
				selected.Image = "registry.example:5000/dev:latest"
			case "unknown platform":
				selected.Platform = "linux/amd64"
			case "missing policy":
				selected.PolicyIdentity = ""
			case "blank policy":
				selected.PolicyIdentity = " "
			case "wrong agent":
				target.Name = "other"
			case "wrong namespace":
				target.Namespace = "other"
			case "empty allowlist":
				p, err = New(Config{Namespace: "kagent", Agents: []string{"writer"}})
				require.NoError(t, err)
				p = p.WithRuntimePlatforms([]string{"linux/arm64"})
			case "no catalog":
				p = original
			}
			ctx := auth.AuthSessionTo(t.Context(), environmentSession{principal})
			if name == "internal" {
				ctx = auth.AuthSessionTo(t.Context(), auth.ControlPlaneSession{})
			}
			require.Equal(t, name == "allowed", p.CheckDevelopmentEnvironment(ctx, target, selected) == nil)
		})
	}
	for _, registry := range []string{"https://registry.example", "registry.example/path", "user@registry.example", "registry.example?x=1", "registry.example#fragment", "", "Registry.example"} {
		_, err = New(Config{Namespace: "kagent", Agents: []string{"writer"}, DevelopmentEnvironmentRegistries: []string{registry}})
		require.Error(t, err, registry)
	}
	require.Error(t, policy.Check(auth.AuthSessionTo(t.Context(), auth.ControlPlaneSession{}), auth.Principal{}, auth.VerbCreate, auth.Resource{Type: "DevelopmentEnvironment", Namespace: "kagent", Name: selection.Image}))
}
