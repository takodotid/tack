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
