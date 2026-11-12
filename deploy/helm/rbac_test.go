// Package helm tests the chart in deploy/helm/tack. It lives outside the chart's directory, so it is not packaged
// with the chart.
package helm

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	chart     = "tack"
	release   = "tack"
	ledgerNS  = "tack-system"
	tableDoc  = "../../docs/operations.md"
	tableHead = "| # | Resource | Verbs | Namespace | Commands | For |"
	// namedFact is the table's fact on resourceNames, which the test checks word for word.
	namedFact = "| `resourceNames` | Rows 3 and 7 to 13 can name their objects."
	// pruneFact is the table's fact on row 16, which the test checks word for word.
	pruneFact = "| Row 16 | Only `tack prune` needs it. The chart grants it only with `rbac.prune`. |"
	// pruneRow is the row that only rbac.prune grants.
	pruneRow = 16
)

// namedRows are the rows whose rules name their objects, as namedFact says.
var namedRows = []int{3, 7, 8, 9, 10, 11, 12, 13}

// values names objects in two namespaces, every kind of rbac.objects among them.
const values = `rbac:
  create: true
  prune: true
  subjects:
  - {apiGroup: rbac.authorization.k8s.io, kind: Group, name: operators}
  objects:
  - namespace: db
    clusters: [pg-a, pg-b]
    poolers: [pg-a-pooler-rw, pg-b-pooler-rw]
    siteServices: [postgres]
  - namespace: flux-system
    kustomizations: [site-a, site-b]
    gitRepositories: [gitops]
    configMaps: [cluster-vars]
    secrets: [cluster-secrets]
`

// need skips the test when a command is missing, except in CI, which must run it.
func need(t *testing.T, cmd string) {
	t.Helper()
	if _, err := exec.LookPath(cmd); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not installed; CI must run this test", cmd)
		}
		t.Skipf("%s is not installed", cmd)
	}
}

// readChart reads each file of the chart, so that go test's cache knows them: helm reads them in its own process.
func readChart(t *testing.T) {
	t.Helper()
	err := filepath.WalkDir(chart, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		_, err = os.ReadFile(p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// helm runs helm with args and the values in vals, written to a file, and returns its output.
func helm(t *testing.T, vals string, args ...string) (string, error) {
	t.Helper()
	need(t, "helm")
	readChart(t)
	if vals != "" {
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, []byte(vals), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", f)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	return string(out), err
}

func render(t *testing.T, vals string) string {
	t.Helper()
	out, err := helm(t, vals, "template", release, chart, "--namespace", ledgerNS)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return out
}

// grant is one verb on one resource, or on one non-resource URL, in one scope: "cluster" for the ClusterRole,
// "ledger" for the Role in the ledger namespace, "objects" for the Roles in the namespaces of rbac.objects.
type grant struct {
	Scope, Group, Resource, Verb string
	// Names are the rule's resourceNames, sorted.
	Names string
}

func (g grant) String() string {
	return fmt.Sprintf("%s %s %q %s [%s]", g.Scope, g.Verb, g.Group, g.Resource, g.Names)
}

// row is one row of the permissions table.
type row struct {
	N         int
	Group     string
	Resources []string
	URLs      []string
	Names     []string
	Verbs     []string
	Scope     string
}

var (
	resourceRE = regexp.MustCompile("^((?:`[^`]+`(?:, )?)+)(?: \\(`([^`]+)`\\))?(.*)$")
	tokenRE    = regexp.MustCompile("`([^`]+)`")
	nameRE     = regexp.MustCompile("^name `([^`]+)`$")
)

// table reads the permissions table of docs/operations.md. It fails on a row it does not understand, and unless
// the rows are numbered 1, 2, 3 and on.
func table(t *testing.T) []row {
	t.Helper()
	b, err := os.ReadFile(tableDoc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(namedFact)) {
		t.Fatalf("%s no longer says %q: update namedRows and this test", tableDoc, namedFact)
	}
	if !bytes.Contains(b, []byte(pruneFact)) {
		t.Fatalf("%s no longer says %q: update pruneRow and this test", tableDoc, pruneFact)
	}
	var rows []row
	groups := map[string]string{}
	in := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := sc.Text()
		switch {
		case l == tableHead:
			in = true
			continue
		case !in || strings.HasPrefix(l, "|---"):
			continue
		case !strings.HasPrefix(l, "|"):
			in = false
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		if len(cells) != 6 {
			t.Fatalf("row %q has %d cells, want 6", l, len(cells))
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		r := row{Verbs: strings.Split(cells[2], ", ")}
		if r.N, err = strconv.Atoi(cells[0]); err != nil || r.N != len(rows)+1 {
			t.Fatalf("row %q: number %q, want %d", l, cells[0], len(rows)+1)
		}
		switch cells[3] {
		case "":
			r.Scope = "cluster"
		case "the ledger's":
			r.Scope = "ledger"
		default:
			r.Scope = "objects"
		}
		res := cells[1]
		switch {
		case res == "the same":
			last := rows[len(rows)-1]
			r.Group, r.Resources = last.Group, last.Resources
		case strings.HasPrefix(res, "non-resource URL"):
			rest := strings.TrimPrefix(strings.TrimPrefix(res, "non-resource URLs "), "non-resource URL ")
			below := strings.HasSuffix(rest, " and below")
			for _, m := range tokenRE.FindAllStringSubmatch(strings.TrimSuffix(rest, " and below"), -1) {
				r.URLs = append(r.URLs, m[1])
				if below {
					r.URLs = append(r.URLs, m[1]+"/*")
				}
			}
			if len(r.URLs) == 0 || strings.Trim(tokenRE.ReplaceAllString(strings.TrimSuffix(rest, " and below"), ""), ", and") != "" {
				t.Fatalf("row %d: cannot read the URLs of %q", r.N, res)
			}
		default:
			m := resourceRE.FindStringSubmatch(res)
			if m == nil {
				t.Fatalf("row %d: cannot read the resources of %q", r.N, res)
			}
			for _, tok := range tokenRE.FindAllStringSubmatch(m[1], -1) {
				r.Resources = append(r.Resources, tok[1])
			}
			r.Group = m[2]
			if n := nameRE.FindStringSubmatch(m[3]); n != nil {
				r.Names = []string{n[1]}
			} else if strings.Contains(m[3], "`") && !strings.HasPrefix(m[3], " named in") {
				t.Fatalf("row %d: cannot read %q", r.N, m[3])
			}
			for _, res := range r.Resources {
				base, _, _ := strings.Cut(res, "/")
				switch {
				case r.Group != "":
					groups[base] = r.Group
				default:
					// A resource without a group takes the group an earlier row gave it, else the core group.
					r.Group = groups[base]
				}
			}
		}
		for _, v := range r.Verbs {
			if !regexp.MustCompile(`^[a-z]+$`).MatchString(v) {
				t.Fatalf("row %d: verb %q", r.N, v)
			}
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		t.Fatalf("no permissions table in %s", tableDoc)
	}
	return rows
}

// objectNames are the names that values gives each kind of rbac.objects, by resource. A Pooler's Deployment has
// the Pooler's name.
var objectNames = map[string][]string{
	"clusters":        {"pg-a", "pg-b"},
	"poolers":         {"pg-a-pooler-rw", "pg-b-pooler-rw"},
	"deployments":     {"pg-a-pooler-rw", "pg-b-pooler-rw"},
	"siteservices":    {"postgres"},
	"kustomizations":  {"site-a", "site-b"},
	"gitrepositories": {"gitops"},
	"configmaps":      {"cluster-vars"},
	"secrets":         {"cluster-secrets"},
}

// want returns the grants the table asks for, with the names that values gives.
func want(t *testing.T, rows []row) []grant {
	t.Helper()
	var out []grant
	for _, r := range rows {
		named := slices.Contains(namedRows, r.N)
		for _, v := range r.Verbs {
			for _, u := range r.URLs {
				out = append(out, grant{Scope: r.Scope, Resource: u, Verb: v})
			}
			for _, res := range r.Resources {
				g := grant{Scope: r.Scope, Group: r.Group, Resource: res, Verb: v}
				switch {
				case named && r.Names != nil:
					g.Names = strings.Join(r.Names, ",")
				case named:
					names, ok := objectNames[res]
					if !ok {
						t.Fatalf("row %d names its objects, and the test's values name no %s", r.N, res)
					}
					g.Names = strings.Join(names, ",")
				}
				out = append(out, g)
			}
		}
	}
	return sorted(out)
}

// rendered reads the grants of the roles in a rendered chart, and checks that each role is bound to the values'
// subjects.
func rendered(t *testing.T, out string) []grant {
	t.Helper()
	var grants []grant
	roles := map[string]bool{}
	bound := map[string]bool{}
	for _, doc := range strings.Split(out, "\n---\n") {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("%v:\n%s", err, doc)
		}
		key := meta.Kind + " " + meta.Metadata.Namespace + "/" + meta.Metadata.Name
		var rules []rbacv1.PolicyRule
		var scope string
		switch meta.Kind {
		case "ClusterRole":
			var r rbacv1.ClusterRole
			if err := yaml.UnmarshalStrict([]byte(doc), &r); err != nil {
				t.Fatal(err)
			}
			rules, scope = r.Rules, "cluster"
		case "Role":
			var r rbacv1.Role
			if err := yaml.UnmarshalStrict([]byte(doc), &r); err != nil {
				t.Fatal(err)
			}
			rules, scope = r.Rules, "objects"
			if r.Namespace == ledgerNS {
				scope = "ledger"
			}
		case "ClusterRoleBinding", "RoleBinding":
			var b rbacv1.RoleBinding
			if err := yaml.UnmarshalStrict([]byte(doc), &b); err != nil {
				t.Fatal(err)
			}
			want := []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "Group", Name: "operators"}}
			if !reflect.DeepEqual(b.Subjects, want) {
				t.Errorf("%s binds %v, want %v", key, b.Subjects, want)
			}
			role, kind := "Role "+b.Namespace+"/"+b.RoleRef.Name, "Role"
			if meta.Kind == "ClusterRoleBinding" {
				role, kind = "ClusterRole /"+b.RoleRef.Name, "ClusterRole"
			}
			if b.RoleRef.Kind != kind {
				t.Errorf("%s binds a %s", key, b.RoleRef.Kind)
			}
			bound[role] = true
			continue
		default:
			continue
		}
		roles[key] = true
		for _, r := range rules {
			names := slices.Clone(r.ResourceNames)
			sort.Strings(names)
			for _, v := range r.Verbs {
				for _, u := range r.NonResourceURLs {
					grants = append(grants, grant{Scope: scope, Resource: u, Verb: v})
				}
				for _, g := range r.APIGroups {
					for _, res := range r.Resources {
						grants = append(grants, grant{Scope: scope, Group: g, Resource: res, Verb: v, Names: strings.Join(names, ",")})
					}
				}
			}
		}
	}
	for role := range roles {
		if !bound[role] {
			t.Errorf("%s is not bound", role)
		}
	}
	return grants
}

func sorted(gs []grant) []grant {
	sort.Slice(gs, func(i, j int) bool { return gs[i].String() < gs[j].String() })
	return gs
}

// TestRolesMatchPermissionsTable: with rbac.create, the chart's roles grant exactly the rows of the permissions
// table in docs/operations.md, each in its scope, and name the objects of each row that can name them, so the two
// cannot drift apart ([RFC#0009] Requirement 29).
func TestRolesMatchPermissionsTable(t *testing.T) {
	got := sorted(rendered(t, render(t, values)))
	exp := want(t, table(t))
	if !reflect.DeepEqual(got, exp) {
		missing, extra := diff(exp, got), diff(got, exp)
		t.Errorf("the chart's roles differ from the permissions table:\nmissing from the chart:\n  %s\nnot in the table:\n  %s",
			strings.Join(missing, "\n  "), strings.Join(extra, "\n  "))
	}
}

func diff(a, b []grant) []string {
	var out []string
	for _, g := range a {
		if !slices.Contains(b, g) {
			out = append(out, g.String())
		}
	}
	return out
}

// TestPruneIsOptional: row 16, delete on sitefailovers for tack prune, is granted only with rbac.prune, and then in
// the ledger Role ([RFC#0003] Requirement 27, [RFC#0009] Requirement 29).
func TestPruneIsOptional(t *testing.T) {
	var row16 []grant
	for _, g := range want(t, table(t)) {
		if g.Verb == "delete" {
			row16 = append(row16, g)
		}
	}
	if len(row16) != 1 || row16[0] != (grant{Scope: "ledger", Group: "failover.tako.id", Resource: "sitefailovers", Verb: "delete"}) {
		t.Fatalf("row %d of the table: %v, want delete on sitefailovers in the ledger namespace", pruneRow, row16)
	}
	without := strings.Replace(values, "  prune: true\n", "", 1)
	for name, vals := range map[string]string{"no prune": without, "prune false": strings.Replace(values, "prune: true", "prune: false", 1)} {
		got := rendered(t, render(t, vals))
		if slices.Contains(got, row16[0]) {
			t.Errorf("%s: the chart grants %s", name, row16[0])
		}
		if exp := slices.DeleteFunc(want(t, table(t)), func(g grant) bool { return g == row16[0] }); !reflect.DeepEqual(sorted(got), exp) {
			t.Errorf("%s: the roles differ from the table without row %d", name, pruneRow)
		}
	}
}

// TestRolesAreOptional: the chart creates no role by default, also from a release whose values have no rbac key,
// such as one installed by an earlier chart.
func TestRolesAreOptional(t *testing.T) {
	for name, vals := range map[string]string{
		"default":      "",
		"no rbac key":  "rbac: null\n",
		"create false": "rbac: {create: false, objects: [{namespace: db}]}\n",
	} {
		out := render(t, vals)
		for _, kind := range []string{"Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding"} {
			if strings.Contains(out, "\nkind: "+kind+"\n") {
				t.Errorf("%s: the chart renders a %s", name, kind)
			}
		}
		if !strings.Contains(out, "\nkind: CustomResourceDefinition\n") {
			t.Errorf("%s: the chart renders no CRD", name)
		}
	}
}

// TestRolesNameTheirObjects: an rbac.objects entry without a namespace, without names, or with the namespace of an
// earlier entry fails the render; a kind without names gets no rule; no subjects binds nothing.
func TestRolesNameTheirObjects(t *testing.T) {
	for vals, want := range map[string]string{
		"rbac: {create: true, objects: [{clusters: [pg-a]}]}\n":                                               "rbac.objects[0] needs a namespace",
		"rbac: {create: true, objects: [{namespace: db}]}\n":                                                  "rbac.objects[0] names no object",
		"rbac: {create: true, objects: [{namespace: db, secrets: []}]}\n":                                     "rbac.objects[0] names no object",
		"rbac: {create: true, objects: [{namespace: db, clusters: [pg-a]}, {namespace: db, poolers: [p]}]}\n": "rbac.objects[1] repeats namespace db",
	} {
		out, err := helm(t, vals, "template", release, chart, "--namespace", ledgerNS)
		if err == nil || !strings.Contains(out, want) {
			t.Errorf("%s: helm template %v, want an error with %q:\n%s", vals, err, want, out)
		}
	}
	out := render(t, "rbac: {create: true, objects: [{namespace: db, clusters: [pg-a]}]}\n")
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		if strings.Contains(out, "\nkind: "+kind+"\n") {
			t.Errorf("no subjects: the chart renders a %s", kind)
		}
	}
	for _, res := range []string{"poolers", "deployments", "kustomizations", "gitrepositories", "configmaps", "secrets", "siteservices"} {
		if strings.Contains(out, `resources: ["`+res+`"]`) {
			t.Errorf("a Role grants %s, which rbac.objects does not name", res)
		}
	}
}

// TestChartLints: helm lint --strict passes with the roles on.
func TestChartLints(t *testing.T) {
	if out, err := helm(t, values, "lint", "--strict", chart); err != nil {
		t.Fatalf("helm lint: %v\n%s", err, out)
	}
}

// TestNotesNameTheWriteCheck (#62): each install and upgrade prints the check of who may write ledgers, with a link at
// the chart's release tag, or at main for a chart that is not of a release ([RFC#0009] Requirement 29). The roles
// only grant access, so they cannot keep a GitOps controller from writing ledgers: only that check finds it.
func TestNotesNameTheWriteCheck(t *testing.T) {
	const anchor = "docs/operations.md#who-may-write-ledgers"
	doc, err := os.ReadFile(tableDoc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "\n### Who may write ledgers\n") {
		t.Fatalf("%s has no heading for %s", tableDoc, anchor)
	}
	packaged := t.TempDir()
	if out, err := helm(t, "", "package", chart, "--version", "1.5.0", "--app-version", "1.5.0", "-d", packaged); err != nil {
		t.Fatalf("helm package: %v\n%s", err, out)
	}
	for ref, c := range map[string]string{"main": chart, "v1.5.0": filepath.Join(packaged, "tack-1.5.0.tgz")} {
		out, err := helm(t, "", "install", release, c, "--dry-run=client", "--namespace", ledgerNS)
		if err != nil {
			t.Fatalf("helm install --dry-run: %v\n%s", err, out)
		}
		_, notes, _ := strings.Cut(out, "\nNOTES:\n")
		for _, want := range []string{
			"only the operators may create, update, patch or delete sitefailovers, or update or patch sitefailovers/status, in namespace " + ledgerNS,
			"https://github.com/takodotid/tack/blob/" + ref + "/" + anchor,
		} {
			if !strings.Contains(notes, want) {
				t.Errorf("%s: the notes lack %q:\n%s", ref, want, notes)
			}
		}
	}
}

// #66. crds.keep kept its earlier value across helm upgrade without values, so a release once set to false stayed
// false, and helm uninstall would then delete the CRD and every ledger. Each install and upgrade prints the values in
// force, and warns while crds.keep is false ([RFC#0009] Requirement 29).
func TestNotesPrintCRDValues(t *testing.T) {
	const warning = "WARNING: crds.keep is false, so helm uninstall, or an upgrade with crds.install=false, deletes the CRD and every ledger at this site"
	for _, tt := range []struct {
		set     []string
		want    string
		warning bool
		install bool
	}{
		{want: "The SiteFailover CRD: crds.install=true, crds.keep=true.", install: true},
		{set: []string{"--set", "crds.keep=false"}, want: "The SiteFailover CRD: crds.install=true, crds.keep=false.", warning: true, install: true},
		{set: []string{"--set", "crds.install=false"}, want: "The SiteFailover CRD: crds.install=false, crds.keep=true."},
		{set: []string{"--set", "crds.install=false,crds.keep=false"}, want: "The SiteFailover CRD: crds.install=false, crds.keep=false."},
	} {
		out, err := helm(t, "", append([]string{"install", release, chart, "--dry-run=client", "--namespace", ledgerNS}, tt.set...)...)
		if err != nil {
			t.Fatalf("%v: helm install --dry-run: %v\n%s", tt.set, err, out)
		}
		_, notes, _ := strings.Cut(out, "\nNOTES:\n")
		if !strings.Contains(notes, tt.want) || !strings.Contains(notes, "helm upgrade without values reuses these") {
			t.Errorf("%v: the notes lack %q:\n%s", tt.set, tt.want, notes)
		}
		if strings.Contains(notes, warning) != tt.warning || strings.Contains(notes, "upgrade with --reset-then-reuse-values --set crds.keep=true") != tt.warning {
			t.Errorf("%v: the warning: %v, want %v:\n%s", tt.set, !tt.warning, tt.warning, notes)
		}
		if strings.Contains(notes, "crds.install is false: this release does not install or upgrade the CRD") == tt.install {
			t.Errorf("%v: the note of crds.install is wrong:\n%s", tt.set, notes)
		}
	}
}
