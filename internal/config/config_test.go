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
