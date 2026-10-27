package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/stop"
)

// isolate keeps git away from the user's configuration and identity.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, k := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// git runs a test's own git command and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.invalid",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.invalid")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// remote is a bare repository and a working clone that writes to it.
type remote struct {
	t    *testing.T
	url  string
	work string
}

func newRemote(t *testing.T, files map[string]string) *remote {
	t.Helper()
	root := t.TempDir()
	rm := &remote{t: t, url: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work")}
	git(t, root, "init", "--quiet", "--bare", "-b", "main", rm.url)
	git(t, root, "clone", "--quiet", rm.url, rm.work)
	rm.push(files, "init")
	return rm
}

// push commits files on top of the branch and pushes them.
func (rm *remote) push(files map[string]string, msg string) string {
	t := rm.t
	t.Helper()
	if git(t, rm.work, "ls-remote", "origin", "refs/heads/main") != "" {
		git(t, rm.work, "fetch", "--quiet", "origin")
		git(t, rm.work, "reset", "--quiet", "--hard", "origin/main")
	}
	for name, content := range files {
		p := filepath.Join(rm.work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, rm.work, "add", "-A")
	git(t, rm.work, "commit", "--quiet", "--allow-empty", "-m", msg)
	git(t, rm.work, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	return git(t, rm.work, "rev-parse", "HEAD")
}

func (rm *remote) head() string { return git(rm.t, rm.url, "rev-parse", "refs/heads/main") }

// file returns a file of the remote at rev, byte for byte.
func (rm *remote) file(rev, name string) string {
	rm.t.Helper()
	out, err := exec.Command("git", "--git-dir", rm.url, "show", rev+":"+name).Output()
	if err != nil {
		rm.t.Fatalf("git show %s:%s: %v", rev, name, err)
	}
	return string(out)
}

const clusterA = `# Cluster of site a.
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg
  namespace: db
spec:
  instances: 3 # three members
  replica:
    primary: site-a   # the primary site
    source: site-b
`

const poolers = `apiVersion: v1
kind: ConfigMap
metadata:
  name: other
---
# The read-write pooler.
apiVersion: postgresql.cnpg.io/v1
kind: Pooler
metadata:
  name: pg-rw
spec:
  instances: 2
`

const values = `cnpg:
  clusters:
  - name: pg
    replica:
      primary: site-a # set by Tack
`

func fixture() map[string]string {
	return map[string]string{
		"deploy/site-a/cluster.yaml":  clusterA,
		"deploy/site-a/pooler.yml":    poolers,
		"deploy/site-a/template.yaml": "{{ if .Values.x }}\nkind: Cluster\n{{ end }}\n",
		"deploy/site-a/README.md":     "not YAML\n",
		"deploy/site-ab/cluster.yaml": clusterA,
		"deploy/site-b/cluster.yaml":  strings.ReplaceAll(clusterA, "site a", "site b"),
		"helm/values-a.yaml":          values,
	}
}

func cnpg(kind, ns, name string) v1alpha1.ObjectRef {
	return v1alpha1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: kind, Namespace: ns, Name: name}
}

func workload(url string) *v1alpha1.WorkloadSpec {
	return &v1alpha1.WorkloadSpec{Git: v1alpha1.GitSpec{
		URL: url, Branch: "main",
		Find: &v1alpha1.FindSpec{Paths: []v1alpha1.SitePath{
			{Site: "a", Path: "deploy/site-a"}, {Site: "b", Path: "deploy/site-b/"},
		}},
		Fields: []v1alpha1.FieldMapping{{
			Site: "a", Object: v1alpha1.ObjectRef{Kind: "Cluster", Name: "pg"}, Field: "spec.replica.primary",
			File: "helm/values-a.yaml", Path: "cnpg.clusters[0].replica.primary",
		}},
		Author: &v1alpha1.GitAuthor{Name: "Tack", Email: "tack@example.invalid"},
	}}
}

func i32(i int32) *int32 { return &i }

// switchover is the change of a switch from site a to site b.
func switchover() []driver.FieldChange {
	return []driver.FieldChange{
		{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Value: str(`"site-b"`)},
		{Site: "b", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Value: str(`"site-b"`)},
		{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.instances", Value: str(`2`)},
		{Site: "a", Object: cnpg("Pooler", "db", "pg-rw"), Field: "spec.instances", Value: str(`0`)},
		{Site: "b", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.promotionToken", Value: nil},
	}
}

func newRepo(t *testing.T) *Repo {
	t.Helper()
	r := New(filepath.Join(t.TempDir(), "work"))
	t.Cleanup(func() { r.Close() })
	return r
}

func TestResolve(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	r := newRepo(t)
	got, err := r.Resolve(context.Background(), workload(rm.url), switchover())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	ch := switchover()
	want := []v1alpha1.GitChange{
		{Site: "a", Object: ch[0].Object, Field: ch[0].Field, Value: ch[0].Value, File: "helm/values-a.yaml", Path: "cnpg.clusters[0].replica.primary"},
		{Site: "b", Object: ch[1].Object, Field: ch[1].Field, Value: ch[1].Value, File: "deploy/site-b/cluster.yaml", Path: "spec.replica.primary", Document: i32(0)},
		{Site: "a", Object: ch[2].Object, Field: ch[2].Field, Value: ch[2].Value, File: "deploy/site-a/cluster.yaml", Path: "spec.instances", Document: i32(0)},
		{Site: "a", Object: ch[3].Object, Field: ch[3].Field, Value: ch[3].Value, File: "deploy/site-a/pooler.yml", Path: "spec.instances", Document: i32(1)},
		{Site: "b", Object: ch[4].Object, Field: ch[4].Field, File: "deploy/site-b/cluster.yaml", Path: "spec.replica.promotionToken", Document: i32(0)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve:\n got %+v\nwant %+v", got, want)
	}
}

func TestResolveFindErrors(t *testing.T) {
	isolate(t)
	files := fixture()
	files["deploy/site-a/pg-copy.yaml"] = strings.Replace(clusterA, "  namespace: db\n", "", 1)
	rm := newRemote(t, files)
	tests := []struct {
		name   string
		spec   func(*v1alpha1.WorkloadSpec)
		change driver.FieldChange
		errs   []string
	}{
		{
			name:   "several manifests in the whole repository",
			spec:   func(w *v1alpha1.WorkloadSpec) { w.Git.Find.Paths = nil },
			change: driver.FieldChange{Site: "b", Object: cnpg("Cluster", "db", "pg"), Field: "spec.instances", Value: str(`1`)},
			errs:   []string{"several manifests", "deploy/site-a/cluster.yaml document 0", "deploy/site-b/cluster.yaml document 0", "spec.git.fields"},
		},
		{
			name:   "a manifest without namespace is not alone",
			change: driver.FieldChange{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.instances", Value: str(`1`)},
			errs:   []string{"several manifests", "deploy/site-a/pg-copy.yaml document 0", "spec.git.fields"},
		},
		{
			name:   "no manifest",
			change: driver.FieldChange{Site: "a", Object: cnpg("Cluster", "db", "other"), Field: "spec.instances", Value: str(`1`)},
			errs:   []string{"no manifest", "deploy/site-a", "deploy/site-a/template.yaml", "spec.git.fields"},
		},
		{
			name:   "another namespace",
			change: driver.FieldChange{Site: "b", Object: cnpg("Cluster", "other", "pg"), Field: "spec.instances", Value: str(`1`)},
			errs:   []string{"no manifest"},
		},
		{
			name:   "another group",
			change: driver.FieldChange{Site: "b", Object: v1alpha1.ObjectRef{APIVersion: "example.com/v1", Kind: "Cluster", Namespace: "db", Name: "pg"}, Field: "spec.instances", Value: str(`1`)},
			errs:   []string{"no manifest"},
		},
		{
			name:   "found manifest cannot take the field",
			change: driver.FieldChange{Site: "b", Object: cnpg("Cluster", "db", "pg"), Field: "spec.instances.x", Value: str(`1`)},
			errs:   []string{"deploy/site-b/cluster.yaml document 0", "not a mapping"},
		},
		{
			name:   "mapped file is missing",
			spec:   func(w *v1alpha1.WorkloadSpec) { w.Git.Fields[0].File = "helm/missing.yaml" },
			change: driver.FieldChange{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Value: str(`"x"`)},
			errs:   []string{"helm/missing.yaml does not exist"},
		},
		{
			name:   "mapped path has no parent",
			spec:   func(w *v1alpha1.WorkloadSpec) { w.Git.Fields[0].Path = "cnpg.clusters[0].replicas.primary" },
			change: driver.FieldChange{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Value: str(`"x"`)},
			errs:   []string{"cnpg.clusters[0].replicas does not exist"},
		},
		{
			name:   "mapped file outside the repository",
			spec:   func(w *v1alpha1.WorkloadSpec) { w.Git.Fields[0].File = "../x.yaml" },
			change: driver.FieldChange{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Value: str(`"x"`)},
			errs:   []string{"not a path inside the repository"},
		},
		{
			name: "two mappings match",
			spec: func(w *v1alpha1.WorkloadSpec) {
				w.Git.Fields = append(w.Git.Fields, w.Git.Fields[0])
			},
			change: driver.FieldChange{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Value: str(`"x"`)},
			errs:   []string{"entries [0 1] all match"},
		},
		{
			name:   "value is not JSON",
			change: driver.FieldChange{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Value: str(`site-b`)},
			errs:   []string{"not JSON"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := workload(rm.url)
			if tt.spec != nil {
				tt.spec(w)
			}
			got, err := newRepo(t).Resolve(context.Background(), w, []driver.FieldChange{tt.change})
			if err == nil {
				t.Fatalf("no error; got %+v", got)
			}
			if got != nil {
				t.Errorf("got changes %+v with the error", got)
			}
			for _, s := range tt.errs {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not contain %q", err, s)
				}
			}
		})
	}
}

func TestResolveMappedRemovalWithoutParent(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	w := workload(rm.url)
	w.Git.Fields[0].Path = "cnpg.clusters[0].replicas.promotionToken"
	w.Git.Fields[0].Field = "spec.replica.promotionToken"
	_, err := newRepo(t).Resolve(context.Background(), w, []driver.FieldChange{
		{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.promotionToken"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestResolveSymlink(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	if err := os.Symlink("values-a.yaml", filepath.Join(rm.work, "helm", "link.yaml")); err != nil {
		t.Fatal(err)
	}
	rm.push(nil, "link")
	w := workload(rm.url)
	w.Git.Fields[0].File = "helm/link.yaml"
	_, err := newRepo(t).Resolve(context.Background(), w, switchover()[:1])
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Resolve: %v, want an error about a regular file", err)
	}
}

func TestResolveNoMapping(t *testing.T) {
	isolate(t)
	w := workload(filepath.Join(t.TempDir(), "missing.git"))
	w.Git.Find = nil
	got, err := newRepo(t).Resolve(context.Background(), w, switchover())
	if !errors.Is(err, ErrNoMapping) {
		t.Fatalf("Resolve: %v, want ErrNoMapping", err)
	}
	if errors.Is(err, ErrUnreachable) || got != nil {
		t.Errorf("Resolve = %+v, %v", got, err)
	}
}

func TestResolveUnreachable(t *testing.T) {
	isolate(t)
	got, err := newRepo(t).Resolve(context.Background(), workload(filepath.Join(t.TempDir(), "missing.git")), switchover())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Resolve: %v, want ErrUnreachable", err)
	}
	if len(got) != len(switchover()) {
		t.Fatalf("Resolve returned %d changes, want %d", len(got), len(switchover()))
	}
	if got[0].File != "helm/values-a.yaml" || got[0].Path != "cnpg.clusters[0].replica.primary" {
		t.Errorf("mapped change = %+v", got[0])
	}
	for _, c := range got[1:] {
		if c.File != "" || c.Document != nil || c.Path != c.Field {
			t.Errorf("searched change = %+v, want no file", c)
		}
	}
}

func TestCommit(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	base := rm.head()
	w := workload(rm.url)
	r := newRepo(t)
	changes, err := r.Resolve(context.Background(), w, switchover())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	commit, err := r.Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "tack: switch pg to b"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Errorf("commit %q is not a full commit id", commit)
	}
	if h := rm.head(); h != commit {
		t.Errorf("remote head %s, want %s", h, commit)
	}
	if p := git(t, rm.url, "rev-parse", commit+"^1"); p != base {
		t.Errorf("parent %s, want %s", p, base)
	}
	if msg := git(t, rm.url, "log", "-1", "--format=%B", commit); msg != "tack: switch pg to b\n\nTack-Run: run-1" {
		t.Errorf("message %q", msg)
	}
	if a := git(t, rm.url, "log", "-1", "--format=%an <%ae>", commit); a != "Tack <tack@example.invalid>" {
		t.Errorf("author %q", a)
	}
	for name, want := range map[string]string{
		"helm/values-a.yaml":          strings.Replace(values, "primary: site-a #", "primary: site-b #", 1),
		"deploy/site-b/cluster.yaml":  strings.Replace(strings.ReplaceAll(clusterA, "site a", "site b"), "primary: site-a ", "primary: site-b ", 1),
		"deploy/site-a/cluster.yaml":  strings.Replace(clusterA, "instances: 3 #", "instances: 2 #", 1),
		"deploy/site-a/pooler.yml":    strings.Replace(poolers, "instances: 2", "instances: 0", 1),
		"deploy/site-ab/cluster.yaml": clusterA,
	} {
		if got := rm.file(commit, name); got != want {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got, want)
		}
	}
	if files := git(t, rm.url, "diff", "--name-only", base, commit); files != "deploy/site-a/cluster.yaml\ndeploy/site-a/pooler.yml\ndeploy/site-b/cluster.yaml\nhelm/values-a.yaml" {
		t.Errorf("changed files:\n%s", files)
	}
}

func TestCommitNothingToChange(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	base := rm.head()
	w := workload(rm.url)
	r := newRepo(t)
	changes, err := r.Resolve(context.Background(), w, []driver.FieldChange{
		{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Value: str(`"site-a"`)},
		{Site: "a", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.promotionToken"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	commit, err := r.Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "m"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if commit != base || rm.head() != base {
		t.Errorf("Commit = %s, head %s; want the base %s", commit, rm.head(), base)
	}
}

func TestCommitSearchesWhenResolveCouldNot(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	base := rm.head()
	w := workload(rm.url)
	changes := []v1alpha1.GitChange{{Site: "b", Object: cnpg("Cluster", "db", "pg"), Field: "spec.replica.primary", Path: "spec.replica.primary", Value: str(`"site-b"`)}}
	commit, err := newRepo(t).Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "m"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := rm.file(commit, "deploy/site-b/cluster.yaml"); !strings.Contains(got, "primary: site-b   # the primary site") {
		t.Errorf("deploy/site-b/cluster.yaml:\n%s", got)
	}
}

func TestCommitAdoptsTheRunsCommit(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	base := rm.head()
	w := workload(rm.url)
	r := newRepo(t)
	changes, err := r.Resolve(context.Background(), w, switchover())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	req := engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "m"}
	first, err := r.Commit(context.Background(), w, req)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// The run was interrupted before it recorded the commit, and resumes
	// in a new process.
	again, err := newRepo(t).Commit(context.Background(), w, req)
	if err != nil {
		t.Fatalf("Commit after interruption: %v", err)
	}
	if again != first || rm.head() != first {
		t.Errorf("Commit = %s, head %s; want %s", again, rm.head(), first)
	}
	// Another run does not adopt it.
	req.Run = "run-2"
	_, err = newRepo(t).Commit(context.Background(), w, req)
	if se, ok := stop.As(err); !ok || se.Reason != v1alpha1.ReasonGitChanged || se.Wait {
		t.Errorf("Commit by another run: %v, want to stop with GitChanged", err)
	}
}

func TestCommitGitChanged(t *testing.T) {
	tests := []struct {
		name    string
		commits []string
	}{
		{name: "a commit of another run", commits: []string{"another change\n\nTack-Run: run-0"}},
		{name: "the run's trailer on another parent", commits: []string{"another change", "copy\n\nTack-Run: run-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			rm := newRemote(t, fixture())
			base := rm.head()
			w := workload(rm.url)
			r := newRepo(t)
			changes, err := r.Resolve(context.Background(), w, switchover())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			var other string
			for i, msg := range tt.commits {
				other = rm.push(map[string]string{"other.txt": strings.Repeat("x", i+1)}, msg)
			}
			_, err = r.Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "m"})
			se, ok := stop.As(err)
			if !ok || se.Reason != v1alpha1.ReasonGitChanged || se.Wait {
				t.Fatalf("Commit: %v, want to stop with GitChanged", err)
			}
			if !strings.Contains(se.Message, base) || !strings.Contains(se.Message, other) {
				t.Errorf("message %q does not name base %s and head %s", se.Message, base, other)
			}
			if rm.head() != other {
				t.Errorf("remote head moved to %s", rm.head())
			}
		})
	}
}

func TestCommitPushRace(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	base := rm.head()
	w := workload(rm.url)
	r := newRepo(t)
	changes, err := r.Resolve(context.Background(), w, switchover())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var other string
	r.beforePush = func() { other = rm.push(map[string]string{"other.txt": "x\n"}, "race") }
	_, err = r.Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "m"})
	se, ok := stop.As(err)
	if !ok || se.Reason != v1alpha1.ReasonGitChanged || se.Wait || !strings.Contains(se.Message, "rejected") {
		t.Fatalf("Commit: %v, want to stop with GitChanged after a rejected push", err)
	}
	if rm.head() != other {
		t.Errorf("remote head %s, want the other commit %s", rm.head(), other)
	}
}

func TestCommitUnreachable(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	base := rm.head()
	w := workload(rm.url)
	r := newRepo(t)
	changes, err := r.Resolve(context.Background(), w, switchover())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.Rename(rm.url, rm.url+".gone"); err != nil {
		t.Fatal(err)
	}
	req := engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "m"}
	for name, repo := range map[string]*Repo{"same clone": r, "new clone": newRepo(t)} {
		_, err := repo.Commit(context.Background(), w, req)
		se, ok := stop.As(err)
		if !ok || se.Reason != v1alpha1.ReasonGitUnreachable || !se.Wait {
			t.Errorf("%s: Commit: %v, want to wait with GitUnreachable", name, err)
		}
	}
}

func TestCommitPushUnreachable(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	base := rm.head()
	w := workload(rm.url)
	r := newRepo(t)
	changes, err := r.Resolve(context.Background(), w, switchover())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r.beforePush = func() {
		if err := os.Rename(rm.url, rm.url+".gone"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = r.Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "m"})
	if se, ok := stop.As(err); !ok || se.Reason != v1alpha1.ReasonGitUnreachable || !se.Wait {
		t.Fatalf("Commit: %v, want to wait with GitUnreachable", err)
	}
	// Git comes back: the retry commits again on the same base.
	if err := os.Rename(rm.url+".gone", rm.url); err != nil {
		t.Fatal(err)
	}
	r.beforePush = nil
	commit, err := r.Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: base, Changes: changes, Message: "m"})
	if err != nil {
		t.Fatalf("Commit after Git came back: %v", err)
	}
	if rm.head() != commit || git(t, rm.url, "rev-parse", commit+"^1") != base {
		t.Errorf("head %s, commit %s, want one commit on %s", rm.head(), commit, base)
	}
}

func TestCommitNoIdentity(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	w := workload(rm.url)
	w.Git.Author = nil
	r := newRepo(t)
	changes, err := r.Resolve(context.Background(), w, switchover())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = r.Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: rm.head(), Changes: changes, Message: "m"})
	if err == nil || !strings.Contains(err.Error(), "spec.git.author") {
		t.Fatalf("Commit: %v, want an error about the identity", err)
	}
}

func TestCommitUsesGitIdentity(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	w := workload(rm.url)
	w.Git.Author = nil
	t.Setenv("GIT_AUTHOR_NAME", "Operator")
	t.Setenv("GIT_AUTHOR_EMAIL", "operator@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Operator")
	t.Setenv("GIT_COMMITTER_EMAIL", "operator@example.invalid")
	r := newRepo(t)
	changes, err := r.Resolve(context.Background(), w, switchover())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	commit, err := r.Commit(context.Background(), w, engine.CommitRequest{Run: "run-1", Base: rm.head(), Changes: changes, Message: "m"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if a := git(t, rm.url, "log", "-1", "--format=%an", commit); a != "Operator" {
		t.Errorf("author %q", a)
	}
}

func TestCheckout(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	first := rm.head()
	w := workload(rm.url)
	r := newRepo(t)
	if _, err := r.Resolve(context.Background(), w, switchover()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The second commit is not in the clone yet.
	second := rm.push(map[string]string{"helm/values-a.yaml": "changed: true\n"}, "second")
	for _, tt := range []struct{ rev, want string }{{first, values}, {second, "changed: true\n"}} {
		dir, err := r.Checkout(context.Background(), w, tt.rev)
		if err != nil {
			t.Fatalf("Checkout(%s): %v", tt.rev, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "helm", "values-a.yaml"))
		if err != nil || string(got) != tt.want {
			t.Errorf("Checkout(%s): values-a.yaml = %q, %v; want %q", tt.rev, got, err, tt.want)
		}
		again, err := r.Checkout(context.Background(), w, tt.rev)
		if err != nil || again != dir {
			t.Errorf("Checkout(%s) again = %s, %v; want %s", tt.rev, again, err, dir)
		}
	}
	if _, err := r.Checkout(context.Background(), w, strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "is not in branch main") {
		t.Errorf("Checkout of an unknown commit: %v", err)
	}
	if _, err := r.Checkout(context.Background(), w, "--help"); err == nil {
		t.Errorf("Checkout of an option: no error")
	}
}

func TestClose(t *testing.T) {
	isolate(t)
	rm := newRemote(t, fixture())
	parent := t.TempDir()
	keep := filepath.Join(parent, "keep")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, work := range []string{parent, filepath.Join(parent, "new")} {
		r := New(work)
		w := workload(rm.url)
		if _, err := r.Resolve(context.Background(), w, switchover()); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if _, err := r.Checkout(context.Background(), w, rm.head()); err != nil {
			t.Fatalf("Checkout: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("after Close the directory holds %v, want only keep", names)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"fetch https://user:secret@git.example.invalid/r.git failed": "fetch https://***@git.example.invalid/r.git failed",
		"https://tok3n@git.example.invalid/r.git":                    "https://***@git.example.invalid/r.git",
		"https://user:p@ss@git.example.invalid/r.git":                "https://***@git.example.invalid/r.git",
		"ssh://git@git.example.invalid/r.git":                        "ssh://***@git.example.invalid/r.git",
		"git@git.example.invalid:r.git":                              "git@git.example.invalid:r.git",
		"https://git.example.invalid/a@b":                            "https://git.example.invalid/a@b",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	e := &gitError{args: []string{"fetch"}, stderr: "fatal: unable to access 'https://user:secret@git.example.invalid/r.git/'"}
	if strings.Contains(e.Error(), "secret") {
		t.Errorf("gitError shows the password: %s", e)
	}
	if _, err := newRepo(t).Resolve(context.Background(), &v1alpha1.WorkloadSpec{Git: v1alpha1.GitSpec{URL: "-https://user:secret@x", Branch: "main", Find: &v1alpha1.FindSpec{}}},
		switchover()); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("Resolve with a bad URL: %v", err)
	}
}
