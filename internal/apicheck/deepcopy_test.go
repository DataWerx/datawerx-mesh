package apicheck

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
)

// apiObjects is every root API object whose deepcopy is hand-written. A new type
// added to either group must be listed here so its deepcopy is guarded too.
func apiObjects() []runtime.Object {
	return []runtime.Object{
		&networkingv1alpha1.MeshPeer{},
		&networkingv1alpha1.MeshPeerList{},
		&networkingv1alpha1.MeshNetworkPolicy{},
		&networkingv1alpha1.MeshNetworkPolicyList{},
		&networkingv1alpha1.EndpointExport{},
		&networkingv1alpha1.EndpointExportList{},
		&mcsv1alpha1.ServiceExport{},
		&mcsv1alpha1.ServiceExportList{},
		&mcsv1alpha1.ServiceImport{},
		&mcsv1alpha1.ServiceImportList{},
	}
}

// opaqueTypes are types we treat as leaves: k8s meta types whose deepcopy is not
// ours to verify. We still check that a slice/map/pointer *holding* them is
// deep-copied (so a forgotten []metav1.Condition copy is caught); we just don't
// descend into their internals.
var opaqueTypes = map[string]bool{
	"k8s.io/apimachinery/pkg/apis/meta/v1.Time":       true,
	"k8s.io/apimachinery/pkg/apis/meta/v1.MicroTime":  true,
	"k8s.io/apimachinery/pkg/apis/meta/v1.Condition":  true,
	"k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta": true,
	"k8s.io/apimachinery/pkg/apis/meta/v1.TypeMeta":   true,
	"k8s.io/apimachinery/pkg/apis/meta/v1.ListMeta":   true,
}

func typeName(t reflect.Type) string { return t.PkgPath() + "." + t.Name() }

func isOpaque(t reflect.Type) bool { return opaqueTypes[typeName(t)] }

// TestDeepCopyRoundTripsAndIsIndependent fully populates every API object,
// deep-copies it, and asserts the copy is both equal and structurally
// independent — no slice, map, or pointer is shared between the original and the
// copy. A reference-typed field omitted from a hand-written DeepCopyInto is
// shallow-copied by the struct assignment, which this catches as aliasing.
func TestDeepCopyRoundTripsAndIsIndependent(t *testing.T) {
	for _, obj := range apiObjects() {
		name := reflect.TypeOf(obj).Elem().Name()
		t.Run(name, func(t *testing.T) {
			fill(reflect.ValueOf(obj).Elem())

			copy := obj.DeepCopyObject()
			if !reflect.DeepEqual(obj, copy) {
				t.Fatalf("%s: deep copy is not equal to the original", name)
			}
			assertNoAliasing(t, name, reflect.ValueOf(obj), reflect.ValueOf(copy))
		})
	}
}

// fill recursively sets every field of v to a non-zero value, allocating
// pointers, one-element slices, and one-entry maps so every reference field is
// exercised. Embedded k8s meta fields and opaque types are left zero; the point
// is the reference fields we declare.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fill(elem)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		fill(key)
		val := reflect.New(v.Type().Elem()).Elem()
		fill(val)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		if isOpaque(v.Type()) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.Anonymous || f.PkgPath != "" {
				continue // skip embedded meta and unexported fields
			}
			fill(v.Field(i))
		}
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
}

// assertNoAliasing walks the original and copy in lockstep, failing if any
// slice, map, or pointer shares a backing pointer between the two.
func assertNoAliasing(t *testing.T, path string, a, b reflect.Value) {
	switch a.Kind() {
	case reflect.Ptr:
		if a.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: pointer is shared between original and copy (shallow copy)", path)
			return
		}
		assertNoAliasing(t, path, a.Elem(), b.Elem())
	case reflect.Slice:
		if a.Len() == 0 {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: slice backing array is shared (field missing from DeepCopyInto)", path)
			return
		}
		for i := 0; i < a.Len(); i++ {
			assertNoAliasing(t, path+"[]", a.Index(i), b.Index(i))
		}
	case reflect.Map:
		if a.IsNil() || a.Len() == 0 {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: map is shared (field missing from DeepCopyInto)", path)
		}
	case reflect.Struct:
		if isOpaque(a.Type()) {
			return
		}
		for i := 0; i < a.NumField(); i++ {
			f := a.Type().Field(i)
			if f.PkgPath != "" {
				continue
			}
			assertNoAliasing(t, path+"."+f.Name, a.Field(i), b.Field(i))
		}
	}
}
