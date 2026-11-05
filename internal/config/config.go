// Package config reads the Workload file.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8syaml "sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// Kind is the kind of the Workload file.
const Kind = "Workload"

// DeprecatedAPIVersion is the Workload's apiVersion before v1, with the same
// schema. Tack still reads it; the CLI warns.
const DeprecatedAPIVersion = "failover.tako.id/v1alpha1"

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

	Spec failoverv1.WorkloadSpec `json:"spec"`
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
	if err := k8syaml.UnmarshalStrict(b, &w); err != nil {
		return nil, fmt.Errorf("workload: %w", err)
	}
	if (w.APIVersion != failoverv1.GroupVersion.String() && w.APIVersion != DeprecatedAPIVersion) || w.Kind != Kind {
		return nil, fmt.Errorf("workload: want apiVersion %s and kind %s, got %q and %q",
			failoverv1.GroupVersion.String(), Kind, w.APIVersion, w.Kind)
	}
	if err := checkSiteNames(b, &w.Spec); err != nil {
		return nil, err
	}
	Default(&w.Spec)
	if err := Validate(w.Name, &w.Spec); err != nil {
		return nil, err
	}
	return &w, nil
}

// checkSiteNames refuses a site name, or a reference to one, written as a
// plain YAML scalar that the decoder reads as other text: YAML 1.1 reads n,
// on or yes as a boolean, and 010 or 1e3 as a number, which become "false",
// "true", "8" or "1000". The site would then answer to neither --to n nor
// the name the operator wrote (#35 row 50). A quoted name is kept as
// written.
func checkSiteNames(b []byte, s *failoverv1.WorkloadSpec) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil || len(doc.Content) == 0 {
		// The strict decode before read the file.
		return nil
	}
	spec := child(doc.Content[0], "spec")
	var errs []string
	check := func(path string, n *yaml.Node, decoded string) {
		const quoted = yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle
		if n == nil || n.Kind != yaml.ScalarNode || n.Style&quoted != 0 || n.Value == decoded {
			return
		}
		errs = append(errs, fmt.Sprintf("%s: YAML reads %s as %q; quote it, as %q", path, n.Value, decoded, n.Value))
	}
	each := func(list *yaml.Node, key, path string, decoded func(i int) (string, bool)) {
		if list == nil || list.Kind != yaml.SequenceNode {
			return
		}
		for i, item := range list.Content {
			if d, ok := decoded(i); ok {
				check(fmt.Sprintf("%s[%d].%s", path, i, key), child(item, key), d)
			}
		}
	}
	each(child(spec, "sites"), "name", "spec.sites", func(i int) (string, bool) {
		return at(s.Sites, i, func(x failoverv1.Site) string { return x.Name })
	})
	git := child(spec, "git")
	each(child(git, "fields"), "site", "spec.git.fields", func(i int) (string, bool) {
		return at(s.Git.Fields, i, func(x failoverv1.FieldMapping) string { return x.Site })
	})
	if s.Git.Find != nil {
		each(child(child(git, "find"), "paths"), "site", "spec.git.find.paths", func(i int) (string, bool) {
			return at(s.Git.Find.Paths, i, func(x failoverv1.SitePath) string { return x.Site })
		})
	}
	if s.Routing != nil && s.Routing.Strait != nil {
		strait := child(child(spec, "routing"), "strait")
		each(child(strait, "services"), "site", "spec.routing.strait.services", func(i int) (string, bool) {
			return at(s.Routing.Strait.Services, i, func(x failoverv1.StraitService) string { return x.Site })
		})
		each(child(strait, "members"), "site", "spec.routing.strait.members", func(i int) (string, bool) {
			return at(s.Routing.Strait.Members, i, func(x failoverv1.StraitMember) string { return x.Site })
		})
	}
	if len(errs) > 0 {
		return fmt.Errorf("workload: site names must stay as written:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// child returns the value of key in a YAML mapping, or nil.
func child(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// at returns field of the i-th element of list, if there is one.
func at[T any](list []T, i int, field func(T) string) (string, bool) {
	if i >= len(list) {
		return "", false
	}
	return field(list[i]), true
}

// Default fills unset fields.
func Default(s *failoverv1.WorkloadSpec) {
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
func Validate(name string, s *failoverv1.WorkloadSpec) error {
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
	} else if err := checkGitURL(s.Git.URL); err != nil {
		add("spec.git.url %v", err)
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
		members := map[string]bool{}
		for i, m := range s.Routing.Strait.Members {
			if !seen[m.Site] {
				add("spec.routing.strait.members[%d].site %q is not a site", i, m.Site)
			}
			members[m.Site] = true
		}
		for _, site := range s.Sites {
			if !members[site.Name] {
				add("spec.routing.strait.members has no member for site %q", site.Name)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("workload %q:\n  %s", name, strings.Join(errs, "\n  "))
	}
	return nil
}

// checkGitURL refuses a Git URL that holds a credential: a password, or a
// user in an http or https URL, which a Git host may take as a token. The
// URL is copied into every ledger and printed. A user in an ssh URL or an
// scp-like address, such as git@host:path, is a name, not a secret. The
// error never repeats the URL.
func checkGitURL(raw string) error {
	const how = "; give the repository only, and the credential through git's own configuration, such as a credential helper or credential.<url>.username"
	if !strings.Contains(raw, "://") {
		// An scp-like address, [user@]host:path, or a local path.
		if user, _, ok := strings.Cut(raw, "@"); ok && strings.Contains(user, ":") && !strings.Contains(user, "/") {
			return fmt.Errorf("holds a password%s", how)
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if u.User == nil {
		return nil
	}
	if _, ok := u.User.Password(); ok {
		return fmt.Errorf("holds a password%s", how)
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return fmt.Errorf("holds a user name, which a Git host may take as a token%s", how)
	}
	return nil
}

// Site returns the named site.
func Site(s *failoverv1.WorkloadSpec, name string) (*failoverv1.Site, bool) {
	for i := range s.Sites {
		if s.Sites[i].Name == name {
			return &s.Sites[i], true
		}
	}
	return nil, false
}
