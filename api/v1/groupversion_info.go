// Package v1 holds the SiteFailover ledger and the Workload configuration.
// +kubebuilder:object:generate=true
// +groupName=failover.tako.id
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group and version of the ledger.
var GroupVersion = schema.GroupVersion{Group: "failover.tako.id", Version: "v1"}

var (
	schemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the ledger types to a scheme.
	AddToScheme = schemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &SiteFailover{}, &SiteFailoverList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
