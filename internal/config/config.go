// Package config reads the Workload file.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/takodotid/tack/api/v1alpha1"
)

// Kind is the kind of the Workload file.
const Kind = "Workload"

// DefaultLedgerNamespace holds SiteFailover objects when the Workload sets none.
const DefaultLedgerNamespace = "tack-system"

// Default timeouts.
const (
	DefaultDemotion  = 10 * time.Minute
	DefaultPromotion = 10 * time.Minute
	DefaultFlux      = 5 * time.Minute
	DefaultRouting   = 2 * time.Minute
)

// Workload is the operator's description of one workload Tack can switch.
type Workload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec v1alpha1.WorkloadSpec `json:"spec"`
}

// Load reads, defaults and validates a Workload file.
func Load(path string) (*Workload, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes, defaults and validates a Workload.
func Parse(b []byte) (*Workload, error) {
	var w Workload
	if err := yaml.UnmarshalStrict(b, &w); err != nil {
		return nil, fmt.Errorf("workload: %w", err)
	}
	if w.APIVersion != v1alpha1.GroupVersion.String() || w.Kind != Kind {
		return nil, fmt.Errorf("workload: want apiVersion %s and kind %s, got %q and %q",
			v1alpha1.GroupVersion.String(), Kind, w.APIVersion, w.Kind)
	}
	Default(&w.Spec)
	if err := Validate(w.Name, &w.Spec); err != nil {
		return nil, err
	}
	return &w, nil
}

// Default fills unset fields.
func Default(s *v1alpha1.WorkloadSpec) {
	if s.Ledger.Namespace == "" {
		s.Ledger.Namespace = DefaultLedgerNamespace
	}
	t := &s.Timeouts
	set := func(d **metav1.Duration, v time.Duration) {
		if *d == nil {
			*d = &metav1.Duration{Duration: v}
		}
	}
	set(&t.Demotion, DefaultDemotion)
	set(&t.Promotion, DefaultPromotion)
	set(&t.Flux, DefaultFlux)
	set(&t.Routing, DefaultRouting)
}

// Validate checks what the engine relies on. Drivers check their own part.
func Validate(name string, s *v1alpha1.WorkloadSpec) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if name == "" {
		add("metadata.name is required")
	}
	if s.Driver == "" {
		add("spec.driver is required")
	}
	if len(s.Sites) < 2 {
		add("spec.sites needs at least two sites")
	}
	seen := map[string]bool{}
	for i, site := range s.Sites {
		if site.Name == "" {
			add("spec.sites[%d].name is required", i)
		}
		if seen[site.Name] {
			add("spec.sites[%d].name %q is repeated", i, site.Name)
		}
		seen[site.Name] = true
		if site.KubeContext == "" {
			add("spec.sites[%d].kubeContext is required", i)
		}
	}
	if s.Git.URL == "" {
		add("spec.git.url is required")
	}
	if s.Git.Branch == "" {
		add("spec.git.branch is required")
	}
	for i, f := range s.Git.Fields {
		if !seen[f.Site] {
			add("spec.git.fields[%d].site %q is not a site", i, f.Site)
		}
		if f.Field == "" || f.File == "" || f.Path == "" {
			add("spec.git.fields[%d] needs field, file and path", i)
		}
		if f.Object.Kind == "" || f.Object.Name == "" {
			add("spec.git.fields[%d].object needs kind and name", i)
		}
	}
	if s.Git.Find != nil {
		for i, p := range s.Git.Find.Paths {
			if !seen[p.Site] {
				add("spec.git.find.paths[%d].site %q is not a site", i, p.Site)
			}
		}
	}
	if s.Routing != nil && s.Routing.Strait != nil {
		for i, svc := range s.Routing.Strait.Services {
			if !seen[svc.Site] {
				add("spec.routing.strait.services[%d].site %q is not a site", i, svc.Site)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("workload %q:\n  %s", name, strings.Join(errs, "\n  "))
	}
	return nil
}

// Site returns the named site.
func Site(s *v1alpha1.WorkloadSpec, name string) (*v1alpha1.Site, bool) {
	for i := range s.Sites {
		if s.Sites[i].Name == name {
			return &s.Sites[i], true
		}
	}
	return nil, false
}
