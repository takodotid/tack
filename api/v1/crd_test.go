package v1

import (
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"
)

// TestCRDVersions checks the generated CRD: v1 is stored, and v1alpha1 is
// served, deprecated, with the same schema, so the API server converts
// between them without a webhook.
func TestCRDVersions(t *testing.T) {
	b, err := os.ReadFile("../../config/crd/failover.tako.id_sitefailovers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(b, &crd); err != nil {
		t.Fatal(err)
	}
	if s := crd.Spec.Conversion; s != nil && s.Strategy != apiextensionsv1.NoneConverter {
		t.Errorf("conversion strategy is %s, want None", s.Strategy)
	}
	versions := crd.Spec.Versions
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want v1 and v1alpha1", len(versions))
	}
	v1, v1alpha1 := versions[0], versions[1]
	if v1.Name != GroupVersion.Version || !v1.Served || !v1.Storage || v1.Deprecated {
		t.Errorf("first version: %s served %t storage %t deprecated %t, want v1 served and stored", v1.Name, v1.Served, v1.Storage, v1.Deprecated)
	}
	if v1alpha1.Name != "v1alpha1" || !v1alpha1.Served || v1alpha1.Storage || !v1alpha1.Deprecated || v1alpha1.DeprecationWarning == nil {
		t.Errorf("second version: %s served %t storage %t deprecated %t, want v1alpha1 served, not stored, deprecated with a warning", v1alpha1.Name, v1alpha1.Served, v1alpha1.Storage, v1alpha1.Deprecated)
	}
	if !equality.Semantic.DeepEqual(v1.Schema, v1alpha1.Schema) {
		t.Error("v1alpha1's schema differs from v1's")
	}
	if !equality.Semantic.DeepEqual(v1.Subresources, v1alpha1.Subresources) || !equality.Semantic.DeepEqual(v1.AdditionalPrinterColumns, v1alpha1.AdditionalPrinterColumns) {
		t.Error("v1alpha1's subresources or printer columns differ from v1's")
	}
}
