package translator_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestExtraHTTPSOriginsGeneratedSchemas(t *testing.T) {
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
			schema := spec.Properties["extraHTTPSOrigins"]
			require.NotNil(t, schema.MaxItems)
			require.EqualValues(t, 32, *schema.MaxItems)
			require.Equal(t, "set", *schema.XListType)
			var internal apiextensions.JSONSchemaProps
			require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&schema, &internal, nil))
			validator, _, err := schemavalidation.NewSchemaValidator(&internal)
			require.NoError(t, err)
			for _, host := range []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com"} {
				for _, origin := range []string{"https://" + host, "https://" + strings.ToUpper(host) + ":443/"} {
					require.Empty(t, schemavalidation.ValidateCustomResource(field.NewPath("extraHTTPSOrigins"), []any{origin}, validator), origin)
				}
			}
			for _, origin := range []string{"http://pypi.org", "https://*.pypi.org", "https://pypi.org:80", "https://127.0.0.1", "https://[::1]", "https://user@pypi.org", "https://pypi.org/simple", "https://pypi.org?", "https://pypi.org#", "https://" + strings.Repeat("a", 64) + ".org"} {
				require.NotEmpty(t, schemavalidation.ValidateCustomResource(field.NewPath("extraHTTPSOrigins"), []any{origin}, validator), origin)
			}
			require.NotEmpty(t, schemavalidation.ValidateCustomResource(field.NewPath("extraHTTPSOrigins"), make([]any, 33), validator))
			require.Contains(t, spec.XValidations, apiextensionsv1.ValidationRule{Rule: "!has(self.extraHTTPSOrigins) || has(self.codex) || has(self.claude)", Message: "extraHTTPSOrigins is supported only by the codex and claude harnesses"})
			// Credential overlap needs resolved external inputs, so compiler tests own it.
		})
	}
}
