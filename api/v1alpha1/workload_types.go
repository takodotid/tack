package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadSpec describes the workload's sites, its Git repository and its ledger.
type WorkloadSpec struct {
	// Driver is the built-in driver that knows the workload's API.
	// +kubebuilder:validation:Enum=cnpg
	Driver string `json:"driver"`

	// Sites lists every site of the workload.
	// +kubebuilder:validation:MinItems=2
	// +listType=map
	// +listMapKey=name
	Sites []Site `json:"sites"`

	// Git is the GitOps repository that Flux applies at every site.
	Git GitSpec `json:"git"`

	// Ledger sets where SiteFailover objects live.
	// +optional
	Ledger LedgerSpec `json:"ledger,omitempty"`

	// Routing confirms that routing followed the switch.
	// +optional
	Routing *RoutingSpec `json:"routing,omitempty"`

	// Timeouts bound the waits of a run.
	// +optional
	Timeouts Timeouts `json:"timeouts,omitempty"`
}

// Site is one Kubernetes cluster that runs a member of the workload.
type Site struct {
	// Name identifies the site in Tack's commands and ledger.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// KubeContext is the kubeconfig context Tack uses for this site.
	KubeContext string `json:"kubeContext"`

	// CNPG is the site's member for the cnpg driver.
	// +optional
	CNPG *CNPGSite `json:"cnpg,omitempty"`
}

// ObjectKey names a namespaced object.
type ObjectKey struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// CNPGSite is a site's CloudNativePG member.
type CNPGSite struct {
	// Cluster is the site's CloudNativePG Cluster.
	Cluster ObjectKey `json:"cluster"`

	// Pooler is the site's read-write Pooler. It runs only while its
	// Cluster is primary.
	// +optional
	Pooler *CNPGPooler `json:"pooler,omitempty"`
}

// CNPGPooler is a read-write Pooler of one Cluster.
type CNPGPooler struct {
	ObjectKey `json:",inline"`

	// Instances is the Pooler's instances while its Cluster is primary.
	// At every other site the Pooler has 0.
	// +kubebuilder:validation:Minimum=1
	Instances int32 `json:"instances"`
}

// GitSpec is the GitOps repository and how Tack edits it.
type GitSpec struct {
	// URL is the repository, as the git binary takes it.
	URL string `json:"url"`

	// Branch is the branch Flux applies.
	Branch string `json:"branch"`

	// Fields maps object fields to places in the repository.
	// A mapped field is written where its mapping says.
	// +optional
	Fields []FieldMapping `json:"fields,omitempty"`

	// Find writes an unmapped field into the manifest that defines its
	// object. Without it, every changed field needs a mapping.
	// +optional
	Find *FindSpec `json:"find,omitempty"`

	// Author signs Tack's commits. Without it, git's own configuration applies.
	// +optional
	Author *GitAuthor `json:"author,omitempty"`
}

// FieldMapping says where one field of one object lives in the repository.
type FieldMapping struct {
	// Site is the site of the object.
	Site string `json:"site"`

	// Object is the live object.
	Object ObjectRef `json:"object"`

	// Field is the field's path in the live object, such as spec.replica.primary.
	Field string `json:"field"`

	// File is the file, relative to the repository root.
	File string `json:"file"`

	// Path is the field's path in the file, such as spec.replica.primary
	// or a Helm values key. Segments are separated by dots; [N] indexes a list.
	Path string `json:"path"`

	// Document selects the YAML document by index when the file holds
	// several and none is the object itself.
	// +optional
	Document *int32 `json:"document,omitempty"`
}

// FindSpec limits the search for manifests.
type FindSpec struct {
	// Paths are the directories searched, per site. A site without an
	// entry searches the whole repository.
	// +optional
	Paths []SitePath `json:"paths,omitempty"`
}

// SitePath is a directory of the repository that belongs to a site.
type SitePath struct {
	Site string `json:"site"`
	Path string `json:"path"`
}

// GitAuthor is a commit author.
type GitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// LedgerSpec sets where SiteFailover objects live.
type LedgerSpec struct {
	// Namespace holds the SiteFailover objects at every site.
	// +kubebuilder:default=tack-system
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// RoutingSpec confirms that routing followed the switch.
type RoutingSpec struct {
	// Strait reads Strait's status at each site.
	// +optional
	Strait *StraitSpec `json:"strait,omitempty"`
}

// StraitSpec names the Strait objects that route the workload.
type StraitSpec struct {
	// Services are the Strait services that route to the workload's primary, per site.
	// +kubebuilder:validation:MinItems=1
	Services []StraitService `json:"services"`
}

// StraitService is the Strait service of one site.
type StraitService struct {
	Site      string `json:"site"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Timeouts bound the waits of a run. Each is a Go duration such as 5m.
type Timeouts struct {
	// Demotion bounds the wait for the old primary to demote.
	// +optional
	Demotion *metav1.Duration `json:"demotion,omitempty"`
	// Promotion bounds the wait for the new primary to promote.
	// +optional
	Promotion *metav1.Duration `json:"promotion,omitempty"`
	// Flux bounds the wait for a Flux reconcile.
	// +optional
	Flux *metav1.Duration `json:"flux,omitempty"`
	// Routing bounds the wait for routing to follow.
	// +optional
	Routing *metav1.Duration `json:"routing,omitempty"`
}
