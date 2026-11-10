package v1

import (
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"
)

func readCRD(t *testing.T) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	b, err := os.ReadFile("../../config/crd/failover.tako.id_sitefailovers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(b, &crd); err != nil {
		t.Fatal(err)
	}
	return crd
}

// RFC0003 Requirement 2: in every version, spec carries the CEL rule
// self == oldSelf, so the API server refuses a change to a ledger's spec.
func TestCRDSpecIsImmutable(t *testing.T) {
	for _, v := range readCRD(t).Spec.Versions {
		spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
		found := false
		for _, r := range spec.XValidations {
			found = found || r.Rule == "self == oldSelf"
		}
		if !ok || !found {
			t.Errorf("version %s: spec has no rule self == oldSelf: %+v", v.Name, spec.XValidations)
		}
	}
}

// TestCRDVersions checks the generated CRD: v1 is stored, and v1alpha1 is
// served, deprecated, with the same schema, so the API server converts
// between them without a webhook.
func TestCRDVersions(t *testing.T) {
	crd := readCRD(t)
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

// #35 row 49. kubectl get sfo showed a run's state, but not why it
// stopped or waits. Every version lists the Reason after the State
// ([RFC#0003] Requirement 15).
func TestCRDPrintsReason(t *testing.T) {
	for _, v := range readCRD(t).Spec.Versions {
		var names []string
		for _, c := range v.AdditionalPrinterColumns {
			names = append(names, c.Name+"="+c.JSONPath)
		}
		got := strings.Join(names, " ")
		if !strings.Contains(got, "State=.status.state Reason=.status.reason ") {
			t.Errorf("version %s: columns %s, want Reason after State", v.Name, got)
		}
	}
}

// #59 item 3. kubectl get sfo showed an abandoned run as Stopped, its last
// state, with nothing that said it was abandoned. Every version lists the
// columns of [RFC#0003] Requirement 15 in order, the Abandoned column next to
// the State and Reason it qualifies, as PR #60 proposed.
func TestCRDPrintsAbandoned(t *testing.T) {
	want := "Workload string .spec.workload.name, Mode string .spec.mode, From string .spec.from, " +
		"To string .spec.to, State string .status.state, Reason string .status.reason, " +
		"Abandoned date .status.abandoned.at, Phase string .status.phase, Age date .metadata.creationTimestamp"
	for _, v := range readCRD(t).Spec.Versions {
		var cols []string
		for _, c := range v.AdditionalPrinterColumns {
			cols = append(cols, c.Name+" "+c.Type+" "+c.JSONPath)
		}
		if got := strings.Join(cols, ", "); got != want {
			t.Errorf("version %s: columns\n  %s\nwant\n  %s", v.Name, got, want)
		}
	}
}
