// Command crdversions adds the deprecated versions to the CRD that
// controller-gen wrote. Each one is a copy of the storage version, served
// but not stored, so the schemas cannot differ and the API server converts
// between them without a webhook.
package main

import (
	"bytes"
	"fmt"
	"os"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// deprecated lists the versions still served, with the storage version
// they copy.
var deprecated = []string{"v1alpha1"}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: crdversions CRD_FILE")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(b, &crd); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if len(crd.Spec.Versions) != 1 || !crd.Spec.Versions[0].Storage {
		return fmt.Errorf("%s: want one version, the storage version, got %d", path, len(crd.Spec.Versions))
	}
	stored := crd.Spec.Versions[0]
	for _, name := range deprecated {
		v := *stored.DeepCopy()
		v.Name = name
		v.Storage = false
		v.Deprecated = true
		warning := fmt.Sprintf("%s/%s %s is deprecated; use %s/%s", crd.Spec.Group, name, crd.Spec.Names.Kind, crd.Spec.Group, stored.Name)
		v.DeprecationWarning = &warning
		crd.Spec.Versions = append(crd.Spec.Versions, v)
	}
	// Through a map, to drop the empty status that the typed CRD carries.
	j, err := yaml.Marshal(&crd)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := yaml.Unmarshal(j, &m); err != nil {
		return err
	}
	delete(m, "status")
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	// controller-gen starts the file with a document separator; keep it.
	return os.WriteFile(path, append([]byte("---\n"), bytes.TrimPrefix(out, []byte("---\n"))...), 0o644)
}
