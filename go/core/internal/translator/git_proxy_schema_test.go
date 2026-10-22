package translator_test

import (
	"os"
	"testing"

	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemaCEL "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestGitProxyGeneratedSchemasAndCEL(t *testing.T) {
	for _, kind := range []string{"harnesses", "agents"} {
		t.Run(kind, func(t *testing.T) {
			filename := "api.kagent.dev_" + kind + ".yaml"
			source, err := os.ReadFile("../../../api/config/crd/bases/" + filename)
			require.NoError(t, err)
			helm, err := os.ReadFile("../../../../helm/kagent-crds/templates/" + filename)
			require.NoError(t, err)
			require.Equal(t, source, helm)
			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, yaml.Unmarshal(source, &crd))
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			if kind == "agents" {
				spec = spec.Properties["harness"]
			}
			git := spec.Properties["git"]
			var internal apiextensions.JSONSchemaProps
			require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&git, &internal, nil))
			validator, _, err := schemavalidation.NewSchemaValidator(&internal)
			require.NoError(t, err)
			structural, err := schema.NewStructural(&internal)
			require.NoError(t, err)
			require.Empty(t, schema.ValidateStructural(field.NewPath("git"), structural))
			cel := schemaCEL.NewValidator(structural, false, 1000000)
			require.NotNil(t, cel)
			for _, tc := range []struct {
				name  string
				value map[string]any
				valid bool
			}{
				{"legacy", map[string]any{"origins": []any{"gitlab.com"}}, true},
				{"read", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": workspace.ReadProxyOrigin}, true},
				{"both", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": workspace.ReadProxyOrigin, "pushProxyOrigin": workspace.PushProxyOrigin}, true},
				{"push alone", map[string]any{"origins": []any{"github.com"}, "pushProxyOrigin": workspace.PushProxyOrigin}, false},
				{"other identity", map[string]any{"origins": []any{"gitlab.com"}, "readProxyOrigin": workspace.ReadProxyOrigin}, false},
				{"two identities", map[string]any{"origins": []any{"github.com", "gitlab.com"}, "readProxyOrigin": workspace.ReadProxyOrigin}, false},
				{"PAT", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": workspace.ReadProxyOrigin, "credentialSecretRef": map[string]any{"name": "mainloop-git-auth", "key": "authorization"}}, false},
				{"empty read", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": ""}, false},
				{"port", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": workspace.ReadProxyOrigin + ":80"}, false},
				{"swapped", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": workspace.PushProxyOrigin}, false},
				{"loopback", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": "http://127.0.0.1"}, false},
				{"dev both", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": "http://mainloop-git-read.mainloop-dev.svc.cluster.local", "pushProxyOrigin": "http://mainloop-git-push.mainloop-dev.svc.cluster.local"}, true},
				{"cross-instance dev read prod push", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": "http://mainloop-git-read.mainloop-dev.svc.cluster.local", "pushProxyOrigin": workspace.PushProxyOrigin}, false},
				{"cross-instance prod read dev push", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": workspace.ReadProxyOrigin, "pushProxyOrigin": "http://mainloop-git-push.mainloop-dev.svc.cluster.local"}, false},
				{"unknown instance", map[string]any{"origins": []any{"github.com"}, "readProxyOrigin": "http://mainloop-git-read.other.svc.cluster.local"}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					errs := schemavalidation.ValidateCustomResource(field.NewPath("git"), tc.value, validator)
					celErrs, _ := cel.Validate(t.Context(), field.NewPath("git"), structural, tc.value, nil, 10000000)
					require.Equal(t, tc.valid, len(errs) == 0 && len(celErrs) == 0, "schema=%v CEL=%v", errs, celErrs)
					if tc.name == "push alone" || tc.name == "other identity" || tc.name == "two identities" || tc.name == "PAT" ||
						tc.name == "cross-instance dev read prod push" || tc.name == "cross-instance prod read dev push" {
						require.NotEmpty(t, celErrs, "must actually evaluate CEL")
					}
				})
			}
		})
	}
}
