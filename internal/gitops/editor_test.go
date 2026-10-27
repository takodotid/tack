package gitops

import (
	"reflect"
	"strings"
	"testing"
)

func str(s string) *string { return &s }

func doc(i int32) *int32 { return &i }

const cluster = `# Cluster of site a.
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg   # the name
  namespace: db
spec:
  instances: 3 # three members
  replica:
    enabled: true
    primary: site-a    # primary site
    source: site-b

  # Storage follows.
  storage:
    size: 10Gi
`

func TestSetField(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		doc   *int32
		path  string
		value *string
		want  string
	}{
		{
			name: "replace keeps comments", in: cluster, path: "spec.replica.primary", value: str(`"site-b"`),
			want: strings.Replace(cluster, "primary: site-a    #", "primary: site-b    #", 1),
		},
		{
			name: "replace number", in: cluster, path: "spec.instances", value: str(`2`),
			want: strings.Replace(cluster, "instances: 3 #", "instances: 2 #", 1),
		},
		{
			name: "replace boolean", in: cluster, path: "spec.replica.enabled", value: str(`false`),
			want: strings.Replace(cluster, "enabled: true", "enabled: false", 1),
		},
		{
			name: "replace with a string that reads as a boolean", in: cluster, path: "spec.replica.primary", value: str(`"true"`),
			want: strings.Replace(cluster, "primary: site-a ", `primary: "true" `, 1),
		},
		{
			name: "replace with a YAML 1.1 boolean word", in: cluster, path: "spec.replica.primary", value: str(`"on"`),
			want: strings.Replace(cluster, "primary: site-a ", `primary: "on" `, 1),
		},
		{
			name: "replace with a string that reads as a number", in: cluster, path: "spec.replica.primary", value: str(`"123"`),
			want: strings.Replace(cluster, "primary: site-a ", `primary: "123" `, 1),
		},
		{
			name: "replace with an empty string", in: cluster, path: "spec.replica.primary", value: str(`""`),
			want: strings.Replace(cluster, "primary: site-a ", `primary: "" `, 1),
		},
		{
			name: "replace with special characters", in: cluster, path: "spec.replica.primary", value: str(`"a: b # c\n\"d\""`),
			want: strings.Replace(cluster, "primary: site-a ", `primary: "a: b # c\n\"d\"" `, 1),
		},
		{
			name: "replace with a token", in: cluster, path: "spec.replica.primary", value: str(`"eyJhIjoxfQ=="`),
			want: strings.Replace(cluster, "primary: site-a ", `primary: "eyJhIjoxfQ==" `, 1),
		},
		{
			name: "replace with null", in: cluster, path: "spec.replica.primary", value: str(`null`),
			want: strings.Replace(cluster, "primary: site-a ", `primary: null `, 1),
		},
		{
			name: "replace a double-quoted scalar", in: "a: \"x\" # c\nb: 1\n", path: "a", value: str(`"v"`),
			want: "a: v # c\nb: 1\n",
		},
		{
			name: "replace a single-quoted scalar", in: "a: 'it''s' # c\nb: 1\n", path: "a", value: str(`"v"`),
			want: "a: v # c\nb: 1\n",
		},
		{
			name: "replace an empty value", in: "a:   # set later\nb: 1\n", path: "a", value: str(`"x"`),
			want: "a: x   # set later\nb: 1\n",
		},
		{
			name: "equal value changes nothing", in: "a: \"site-a\" # c\n", path: "a", value: str(`"site-a"`),
			want: "a: \"site-a\" # c\n",
		},
		{
			name: "equal number changes nothing", in: "a: 3\n", path: "a", value: str(`3`),
			want: "a: 3\n",
		},
		{
			name: "replace after characters of several bytes", in: "é: ü\nkey: välue # ö\n", path: "key", value: str(`"x"`),
			want: "é: ü\nkey: x # ö\n",
		},
		{
			name: "insert a leaf after the last entry", in: cluster, path: "spec.replica.promotionToken", value: str(`"tok=="`),
			want: strings.Replace(cluster, "    source: site-b\n", "    source: site-b\n    promotionToken: \"tok==\"\n", 1),
		},
		{
			name: "insert before trailing comments and blank lines",
			in:   "spec:\n  replica:\n    primary: a\n    # about replica\n\n  other: 1\n", path: "spec.replica.source", value: str(`"b"`),
			want: "spec:\n  replica:\n    primary: a\n    source: b\n    # about replica\n\n  other: 1\n",
		},
		{
			name: "insert after a nested block value",
			in:   "spec:\n  replica:\n    primary: a\nmetadata: x\n", path: "spec.instances", value: str(`2`),
			want: "spec:\n  replica:\n    primary: a\n  instances: 2\nmetadata: x\n",
		},
		{
			name: "insert creates mappings",
			in:   "spec:\n  instances: 3\nstatus: {}\n", path: "spec.replica.primary", value: str(`"b"`),
			want: "spec:\n  instances: 3\n  replica:\n    primary: b\nstatus: {}\n",
		},
		{
			name: "insert follows the file's indentation",
			in:   "spec:\n    instances: 3\n", path: "spec.replica.primary", value: str(`"b"`),
			want: "spec:\n    instances: 3\n    replica:\n        primary: b\n",
		},
		{
			name: "insert into a list item",
			in:   "cnpg:\n  clusters:\n  - name: a\n    replica:\n      enabled: true\n  - name: b\n", path: "cnpg.clusters[0].replica.primary", value: str(`"site-b"`),
			want: "cnpg:\n  clusters:\n  - name: a\n    replica:\n      enabled: true\n      primary: site-b\n  - name: b\n",
		},
		{
			name: "insert into a list item's mapping",
			in:   "clusters:\n- name: a\n- name: b\n", path: "clusters[1].replica.primary", value: str(`"x"`),
			want: "clusters:\n- name: a\n- name: b\n  replica:\n    primary: x\n",
		},
		{
			name: "replace in a list item",
			in:   "clusters:\n  - name: a\n  - name: b # second\n", path: "clusters[1].name", value: str(`"c"`),
			want: "clusters:\n  - name: a\n  - name: c # second\n",
		},
		{
			name: "insert after a list at the key's column",
			in:   "a:\n  list:\n  - x\n  - y\nb: 1\n", path: "a.more", value: str(`1`),
			want: "a:\n  list:\n  - x\n  - y\n  more: 1\nb: 1\n",
		},
		{
			name: "insert at the end of a file without a final line break",
			in:   "a:\n  b: 1", path: "a.c", value: str(`2`),
			want: "a:\n  b: 1\n  c: 2\n",
		},
		{
			name: "insert keeps CRLF line breaks",
			in:   "a:\r\n  b: 1\r\nz: 0\r\n", path: "a.c", value: str(`2`),
			want: "a:\r\n  b: 1\r\n  c: 2\r\nz: 0\r\n",
		},
		{
			name: "insert a key that needs quotes",
			in:   "a:\n  b: 1\n", path: "a.yes", value: str(`2`),
			want: "a:\n  b: 1\n  \"yes\": 2\n",
		},
		{
			name: "remove a key", in: cluster, path: "spec.replica.source", value: nil,
			want: strings.Replace(cluster, "    source: site-b\n", "", 1),
		},
		{
			name: "remove a key and its block value", in: cluster, path: "spec.replica", value: nil,
			want: strings.Replace(cluster, "  replica:\n    enabled: true\n    primary: site-a    # primary site\n    source: site-b\n", "", 1),
		},
		{
			name: "remove a key and its block scalar",
			in:   "data:\n  script: |\n    echo a\n\n    echo b\n  other: 1\n", path: "data.script", value: nil,
			want: "data:\n  other: 1\n",
		},
		{
			name: "remove a key and its list at the key's column",
			in:   "a:\n  list:\n  - x\n  - y\n  b: 1\n", path: "a.list", value: nil,
			want: "a:\n  b: 1\n",
		},
		{
			name: "remove an absent key changes nothing", in: cluster, path: "spec.replica.promotionToken", value: nil,
			want: cluster,
		},
		{
			name: "remove under an absent parent changes nothing", in: "spec:\n  instances: 3\n", path: "spec.replica.promotionToken", value: nil,
			want: "spec:\n  instances: 3\n",
		},
		{
			name: "remove under a null parent changes nothing", in: "spec:\n  replica:\n  x: 1\n", path: "spec.replica.promotionToken", value: nil,
			want: "spec:\n  replica:\n  x: 1\n",
		},
		{
			name: "remove an absent key in flow style changes nothing", in: "spec: {replica: {primary: a}}\n", path: "spec.replica.promotionToken", value: nil,
			want: "spec: {replica: {primary: a}}\n",
		},
		{
			name: "edit the selected document", in: "a: 1\n---\n# second\nb: 2\nc: 3\n", doc: doc(1), path: "b", value: str(`5`),
			want: "a: 1\n---\n# second\nb: 5\nc: 3\n",
		},
		{
			name: "insert into a document followed by another", in: "a:\n  x: 1\n# end\n---\nb: 2\n", doc: doc(0), path: "a.w", value: str(`2`),
			want: "a:\n  x: 1\n  w: 2\n# end\n---\nb: 2\n",
		},
		{
			name: "a final --- leaves one document", in: "# head\n---\na: 1\n---\n", path: "a", value: str(`2`),
			want: "# head\n---\na: 2\n---\n",
		},
		{
			name: "a byte order mark stays", in: "\xef\xbb\xbfa: 1\nb: 2\n", path: "a", value: str(`3`),
			want: "\xef\xbb\xbfa: 3\nb: 2\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := setField([]byte(tt.in), tt.doc, tt.path, tt.value)
			if err != nil {
				t.Fatalf("setField: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestSetFieldErrors(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		doc   *int32
		path  string
		value *string
		err   string
	}{
		{name: "flow mapping on the path", in: "spec: {replica: {primary: a}}\n", path: "spec.replica.primary", value: str(`"b"`), err: "flow style"},
		{name: "flow mapping to insert into", in: "spec: {}\n", path: "spec.replica", value: str(`"b"`), err: "flow style"},
		{name: "flow list on the path", in: "l: [a, b]\n", path: "l[0]", value: str(`"b"`), err: "flow style"},
		{name: "anchor on the path", in: "a: &x\n  b: 1\nc: *x\n", path: "a.b", value: str(`2`), err: "anchor"},
		{name: "alias on the path", in: "a: &x\n  b: 1\nc: *x\n", path: "c.b", value: str(`2`), err: "alias"},
		{name: "alias as the value", in: "a: &x 1\nc: *x\n", path: "c", value: str(`2`), err: "alias"},
		{name: "merge key hides the field", in: "base: &b\n  x: 1\nc:\n  <<: *b\n  y: 2\n", path: "c.x", value: str(`2`), err: "merge key"},
		{name: "merge key hides a removal", in: "base: &b\n  x: 1\nc:\n  <<: *b\n  y: 2\n", path: "c.x", value: nil, err: "merge key"},
		{name: "block scalar", in: "a: |\n  x\nb: 1\n", path: "a", value: str(`"y"`), err: "block scalar"},
		{name: "folded scalar", in: "a: >\n  x\nb: 1\n", path: "a", value: str(`"y"`), err: "block scalar"},
		{name: "plain scalar on several lines", in: "a: x\n  y\nb: 1\n", path: "a", value: str(`"z"`), err: "several lines"},
		{name: "quoted scalar on several lines", in: "a: \"x\n  y\"\nb: 1\n", path: "a", value: str(`"z"`), err: "several lines"},
		{name: "explicit tag", in: "a: !!str 1\nb: 1\n", path: "a", value: str(`"z"`), err: "explicit tag"},
		{name: "replace a mapping", in: "a:\n  b: 1\n", path: "a", value: str(`"z"`), err: "only a scalar"},
		{name: "missing list item", in: "l:\n- a\n", path: "l[3]", value: str(`"z"`), err: "does not create list items"},
		{name: "missing list under a missing key", in: "a:\n  b: 1\n", path: "a.l[0].x", value: str(`"z"`), err: "does not create list items"},
		{name: "index into a mapping", in: "a:\n  b: 1\n", path: "a[0]", value: str(`"z"`), err: "not a list"},
		{name: "key into a scalar", in: "a: 1\n", path: "a.b", value: str(`"z"`), err: "not a mapping"},
		{name: "insert into null", in: "a:\nb: 1\n", path: "a.c", value: str(`"z"`), err: "empty (null)"},
		{name: "remove the only key", in: "a:\n  b: 1\nc: 2\n", path: "a.b", value: nil, err: "only key"},
		{name: "remove a key that shares its line", in: "l:\n- name: a\n  x: 1\n", path: "l[0].name", value: nil, err: "shares its line"},
		{name: "remove a list item", in: "l:\n- a\n- b\n", path: "l[0]", value: nil, err: "list item"},
		{name: "several documents without an index", in: "a: 1\n---\nb: 2\n", path: "a", value: str(`2`), err: "several YAML documents"},
		{name: "document out of range", in: "a: 1\n", doc: doc(2), path: "a", value: str(`2`), err: "document 2 does not exist"},
		{name: "no document", in: "# nothing\n", path: "a", value: str(`2`), err: "no YAML document"},
		{name: "invalid YAML", in: "a: [\n", path: "a", value: str(`2`), err: "not valid YAML"},
		{name: "value is not JSON", in: "a: 1\n", path: "a", value: str(`site-b`), err: "not JSON"},
		{name: "value is an object", in: "a: 1\n", path: "a", value: str(`{"b": 1}`), err: "only scalars"},
		{name: "value has trailing data", in: "a: 1\n", path: "a", value: str(`1 2`), err: "not one JSON value"},
		{name: "bad path", in: "a: 1\n", path: "a..b", value: str(`2`), err: "empty key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := setField([]byte(tt.in), tt.doc, tt.path, tt.value)
			if err == nil {
				t.Fatalf("no error; got:\n%s", got)
			}
			if !strings.Contains(err.Error(), tt.err) {
				t.Errorf("error %q does not contain %q", err, tt.err)
			}
		})
	}
}

func TestCheckField(t *testing.T) {
	in := []byte("spec:\n  instances: 3\n")
	tests := []struct {
		name           string
		path           string
		remove, parent bool
		err            string
	}{
		{name: "leaf may be absent", path: "spec.replica", parent: true},
		{name: "parent must exist", path: "spec.replica.primary", parent: true, err: "spec.replica does not exist"},
		{name: "parent may be absent without strict", path: "spec.replica.primary"},
		{name: "removal under an absent parent", path: "spec.replica.promotionToken", remove: true, parent: true},
		{name: "existing scalar", path: "spec.instances", parent: true},
		{name: "not a mapping", path: "spec.instances.x", parent: true, err: "not a mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkField(in, nil, tt.path, tt.remove, tt.parent)
			switch {
			case tt.err == "" && err != nil:
				t.Fatalf("checkField: %v", err)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Fatalf("error %v does not contain %q", err, tt.err)
			}
		})
	}
}

func TestParsePath(t *testing.T) {
	good := map[string][]segment{
		"spec.replica.primary":             {{key: "spec", index: -1}, {key: "replica", index: -1}, {key: "primary", index: -1}},
		"cnpg.clusters[0].replica.primary": {{key: "cnpg", index: -1}, {key: "clusters", index: -1}, {index: 0}, {key: "replica", index: -1}, {key: "primary", index: -1}},
		"[1].a":                            {{index: 1}, {key: "a", index: -1}},
		"a[0][12]":                         {{key: "a", index: -1}, {index: 0}, {index: 12}},
	}
	for p, want := range good {
		got, err := parsePath(p)
		if err != nil {
			t.Errorf("parsePath(%q): %v", p, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsePath(%q) = %v, want %v", p, got, want)
		}
		if f := formatPath(got); f != p {
			t.Errorf("formatPath(parsePath(%q)) = %q", p, f)
		}
	}
	for _, p := range []string{"", "a..b", ".a", "a.", "a[", "a[x]", "a[-1]", "a[+1]", "a[]", "a[0]b", "a]", "a.[0]"} {
		if _, err := parsePath(p); err == nil {
			t.Errorf("parsePath(%q): no error", p)
		}
	}
}

func TestRenderJSON(t *testing.T) {
	tests := []struct {
		in, text string
		want     any
	}{
		{`"site-b"`, `site-b`, "site-b"},
		{`"a/b.c_d"`, `a/b.c_d`, "a/b.c_d"},
		{`"Yes"`, `"Yes"`, "Yes"},
		{`"n"`, `"n"`, "n"},
		{`"null"`, `"null"`, "null"},
		{`"1.5"`, `"1.5"`, "1.5"},
		{`"2026-10-27"`, `"2026-10-27"`, "2026-10-27"},
		{`"-a"`, `"-a"`, "-a"},
		{`"a b"`, `"a b"`, "a b"},
		{`"tab\tend\u0001"`, `"tab\tend\u0001"`, "tab\tend\x01"},
		{`"back\\slash"`, `"back\\slash"`, `back\slash`},
		{`"ünï"`, `"ünï"`, "ünï"},
		{`2`, `2`, 2},
		{`-1.5`, `-1.5`, -1.5},
		{`true`, `true`, true},
		{`null`, `null`, nil},
	}
	for _, tt := range tests {
		text, want, err := renderJSON(tt.in)
		if err != nil {
			t.Errorf("renderJSON(%s): %v", tt.in, err)
			continue
		}
		if text != tt.text || !reflect.DeepEqual(want, tt.want) {
			t.Errorf("renderJSON(%s) = %s, %#v; want %s, %#v", tt.in, text, want, tt.text, tt.want)
		}
		// The text reads back as the value.
		got, err := setField([]byte("k: 0\n"), nil, "k", str(tt.in))
		if err != nil {
			t.Errorf("setField with %s: %v", tt.in, err)
		} else if string(got) != "k: "+tt.text+"\n" {
			t.Errorf("setField with %s = %q", tt.in, got)
		}
	}
}
