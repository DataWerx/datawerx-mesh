package apicheck

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
)

// crdCase pairs a Go root type with the hand-written CRD YAML that must describe
// it. The Go type is the source of truth; the CRD schema must cover every field.
type crdCase struct {
	obj      any
	yamlFile string
}

func crdCases() []crdCase {
	return []crdCase{
		{&networkingv1alpha1.MeshPeer{}, "networking.datawerx.io_meshpeers.yaml"},
		{&networkingv1alpha1.MeshNetworkPolicy{}, "networking.datawerx.io_meshnetworkpolicies.yaml"},
		{&networkingv1alpha1.EndpointExport{}, "networking.datawerx.io_endpointexports.yaml"},
		{&mcsv1alpha1.ServiceExport{}, "multicluster.x-k8s.io_serviceexports.yaml"},
		{&mcsv1alpha1.ServiceImport{}, "multicluster.x-k8s.io_serviceimports.yaml"},
	}
}

// apiPkgPrefix bounds the recursion: we descend into our own API struct types
// (where a new field is our responsibility to add to the CRD) but treat external
// types (metav1, corev1) as leaves — their internal fields are not our schema's
// concern.
const apiPkgPrefix = "github.com/DataWerx/datawerx-mesh/pkg/apis"

// TestCRDSchemaCoversEveryGoField asserts that every JSON field name on a CRD's
// Go type appears as a property somewhere in its hand-written CRD schema. It is
// the guard for the most dangerous drift: a field added to the Go type but not
// the CRD YAML, which the API server would silently strip.
func TestCRDSchemaCoversEveryGoField(t *testing.T) {
	for _, tc := range crdCases() {
		typ := reflect.TypeOf(tc.obj).Elem()
		t.Run(typ.Name(), func(t *testing.T) {
			schemaNames := schemaPropertyNames(t, tc.yamlFile)
			goNames := map[string]bool{}
			collectGoFieldNames(typ, goNames)

			for name := range goNames {
				if !schemaNames[name] {
					t.Errorf("Go field %q on %s is not described in %s — update the CRD schema",
						name, typ.Name(), tc.yamlFile)
				}
			}
		})
	}
}

// collectGoFieldNames gathers the JSON field names declared on t and, for fields
// whose (dereferenced) type is one of our own API structs, recursively on those.
// Embedded meta fields (TypeMeta/ObjectMeta/ListMeta) are skipped — they are not
// described as named properties beyond apiVersion/kind/metadata.
func collectGoFieldNames(t reflect.Type, out map[string]bool) {
	t = deref(t)
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous || f.PkgPath != "" {
			continue
		}
		name := jsonName(f)
		if name == "" || name == "-" {
			continue
		}
		out[name] = true

		if elem := deref(f.Type); elem.Kind() == reflect.Slice || elem.Kind() == reflect.Array {
			elem = deref(elem.Elem())
		}
		if elem := structElem(f.Type); elem != nil && strings.HasPrefix(elem.PkgPath(), apiPkgPrefix) {
			collectGoFieldNames(elem, out)
		}
	}
}

// structElem returns the underlying struct type of t after unwrapping pointers
// and slices/arrays, or nil if there isn't one.
func structElem(t reflect.Type) reflect.Type {
	t = deref(t)
	for t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = deref(t.Elem())
	}
	if t.Kind() == reflect.Struct {
		return t
	}
	return nil
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// jsonName returns the JSON field name from the struct tag, before any options.
func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		return tag[:i]
	}
	return tag
}

// schemaPropertyNames parses a CRD YAML and returns the set of every property
// name appearing anywhere in its served version's OpenAPI schema.
func schemaPropertyNames(t *testing.T, file string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", file))
	if err != nil {
		t.Fatalf("reading CRD %s: %v", file, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing CRD %s: %v", file, err)
	}
	names := map[string]bool{}
	for _, v := range crd.Spec.Versions {
		if !v.Served || v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		walkSchema(*v.Schema.OpenAPIV3Schema, names)
	}
	if len(names) == 0 {
		t.Fatalf("CRD %s yielded no schema properties — is a served version present?", file)
	}
	return names
}

// walkSchema collects every property name reachable from a JSON schema node.
func walkSchema(s apiextensionsv1.JSONSchemaProps, out map[string]bool) {
	for name, prop := range s.Properties {
		out[name] = true
		walkSchema(prop, out)
	}
	if s.Items != nil {
		if s.Items.Schema != nil {
			walkSchema(*s.Items.Schema, out)
		}
		for _, js := range s.Items.JSONSchemas {
			walkSchema(js, out)
		}
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
		walkSchema(*s.AdditionalProperties.Schema, out)
	}
}
