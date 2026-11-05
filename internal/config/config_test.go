package config

import (
	"strings"
	"testing"
)

const workload = `kind: Workload
metadata: {name: pg-main}
spec:
  driver: cnpg
  sites:
  - {name: a, kubeContext: site-a}
  - {name: b, kubeContext: site-b}
  git: {url: https://git.example.com/gitops.git, branch: main}
`

func TestParseAPIVersion(t *testing.T) {
	for _, tt := range []struct {
		apiVersion string
		err        string
	}{
		{apiVersion: "failover.tako.id/v1"},
		{apiVersion: "failover.tako.id/v1alpha1"},
		{apiVersion: "failover.tako.id/v1beta1", err: `want apiVersion failover.tako.id/v1 and kind Workload, got "failover.tako.id/v1beta1"`},
	} {
		w, err := Parse([]byte("apiVersion: " + tt.apiVersion + "\n" + workload))
		switch {
		case tt.err == "" && err != nil:
			t.Errorf("%s: %v", tt.apiVersion, err)
		case tt.err == "" && w.APIVersion != tt.apiVersion:
			t.Errorf("%s: parsed as %s", tt.apiVersion, w.APIVersion)
		case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
			t.Errorf("%s: got error %v, want %q", tt.apiVersion, err, tt.err)
		}
	}
}

// A Git URL that holds a credential is refused, and the error does not
// repeat it. Issue #31.
func TestParseGitURL(t *testing.T) {
	for _, tt := range []struct {
		url string
		err string
	}{
		{url: "https://git.example.com/gitops.git"},
		{url: "ssh://git@git.example.com/gitops.git"},
		{url: "git@git.example.com:gitops.git"},
		{url: "/srv/git/gitops.git"},
		{url: "file:///srv/git/gitops.git"},
		{url: "https://user:FAKE-TOKEN-123@git.example.com/gitops.git", err: "holds a password"},
		{url: "http://user:FAKE-TOKEN-123@git.example.com/gitops.git", err: "holds a password"},
		{url: "ssh://git:FAKE-TOKEN-123@git.example.com/gitops.git", err: "holds a password"},
		{url: "git:FAKE-TOKEN-123@git.example.com:gitops.git", err: "holds a password"},
		{url: "https://FAKE-TOKEN-123@git.example.com/gitops.git", err: "holds a user name"},
		{url: "https://oauth2:FAKE-TOKEN-123@git.example.com/gitops.git", err: "holds a password"},
	} {
		src := strings.Replace("apiVersion: failover.tako.id/v1\n"+workload, "https://git.example.com/gitops.git", tt.url, 1)
		_, err := Parse([]byte(src))
		switch {
		case tt.err == "" && err != nil:
			t.Errorf("%s: %v", tt.url, err)
		case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
			t.Errorf("%s: got %v, want %q", tt.url, err, tt.err)
		case err != nil && strings.Contains(err.Error(), "FAKE-TOKEN-123"):
			t.Errorf("%s: the error repeats the credential: %v", tt.url, err)
		}
	}
}

// RFC0008 Requirement 2: each SiteService and member names a site of the
// workload, and every site has a member.
func TestParseRouting(t *testing.T) {
	for _, tt := range []struct {
		name    string
		routing string
		err     string
	}{
		{name: "every site has a member", routing: `{services: [{site: a, namespace: apps, name: pg}], members: [{site: a, member: pg-a}, {site: b, member: pg-b}]}`},
		{name: "a service at another site", routing: `{services: [{site: c, namespace: apps, name: pg}], members: [{site: a, member: pg-a}, {site: b, member: pg-b}]}`,
			err: `spec.routing.strait.services[0].site "c" is not a site`},
		{name: "a member at another site", routing: `{services: [{site: a, namespace: apps, name: pg}], members: [{site: a, member: pg-a}, {site: b, member: pg-b}, {site: c, member: pg-c}]}`,
			err: `spec.routing.strait.members[2].site "c" is not a site`},
		{name: "a site without a member", routing: `{services: [{site: a, namespace: apps, name: pg}], members: [{site: a, member: pg-a}]}`,
			err: `spec.routing.strait.members has no member for site "b"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte("apiVersion: failover.tako.id/v1\n" + workload + "  routing: {strait: " + tt.routing + "}\n"))
			switch {
			case tt.err == "" && err != nil:
				t.Errorf("%v", err)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Errorf("got %v, want %q", err, tt.err)
			}
		})
	}
}

// #35 row 50 (RFC0009 Requirement 11). YAML 1.1 reads an unquoted n, on
// or yes as a boolean and 010 as a number, so a site named so became
// "false", "true" or "8", and --to n then failed. Such a name, or a
// reference to one, is refused unquoted, and kept as written quoted.
func TestParseSiteNames(t *testing.T) {
	sites := func(a, b string) string {
		return strings.Replace(strings.Replace(workload, "{name: a,", "{name: "+a+",", 1), "{name: b,", "{name: "+b+",", 1)
	}
	for _, tt := range []struct {
		name  string
		src   string
		err   string
		names []string
	}{
		{name: "plain names", src: sites("a", "b"), names: []string{"a", "b"}},
		{name: "n", src: sites("n", "b"), err: `spec.sites[0].name: YAML reads n as "false"; quote it, as "n"`},
		{name: "on and yes", src: sites("on", "yes"), err: `spec.sites[0].name: YAML reads on as "true"; quote it, as "on"` + "\n  " + `spec.sites[1].name: YAML reads yes as "true"; quote it, as "yes"`},
		{name: "a number", src: sites("010", "b"), err: `spec.sites[0].name: YAML reads 010 as "8"; quote it, as "010"`},
		{name: "quoted", src: sites(`"n"`, `'on'`), names: []string{"n", "on"}},
		{name: "a quoted false", src: sites(`"false"`, "b"), names: []string{"false", "b"}},
		{name: "a reference", src: sites(`"n"`, "b") + "  routing: {strait: {services: [{site: n, namespace: apps, name: pg}], members: [{site: \"n\", member: pg-a}, {site: b, member: pg-b}]}}\n",
			err: `spec.routing.strait.services[0].site: YAML reads n as "false"; quote it, as "n"`},
		{name: "a Git mapping", src: strings.Replace(sites(`"y"`, "b"), "branch: main}", "branch: main, fields: [{site: y, object: {kind: Cluster, name: pg}, field: spec.x, file: f.yaml, path: x}]}", 1),
			err: `spec.git.fields[0].site: YAML reads y as "true"; quote it, as "y"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, err := Parse([]byte("apiVersion: failover.tako.id/v1\n" + tt.src))
			switch {
			case tt.err != "":
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("got %v, want %q", err, tt.err)
				}
			case err != nil:
				t.Fatal(err)
			default:
				for i, n := range tt.names {
					if got := w.Spec.Sites[i].Name; got != n {
						t.Errorf("site %d is %q, want %q", i, got, n)
					}
				}
			}
		})
	}
}
