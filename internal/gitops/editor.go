package gitops

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The editor sets one field of a YAML file and changes only the bytes of
// that field. yaml.v3 finds the nodes; the bytes are then spliced, and the
// result is parsed again to check that nothing else changed.

// segment is one step of a path: a mapping key, or a list index when index
// is not negative.
type segment struct {
	key   string
	index int
}

func (s segment) String() string {
	if s.index >= 0 {
		return "[" + strconv.Itoa(s.index) + "]"
	}
	return s.key
}

// formatPath writes segments as a path.
func formatPath(segs []segment) string {
	var b strings.Builder
	for i, s := range segs {
		if s.index < 0 && i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.String())
	}
	return b.String()
}

// where names the node at segs for a message.
func where(segs []segment) string {
	if len(segs) == 0 {
		return "the document root"
	}
	return formatPath(segs)
}

// parsePath splits a path such as cnpg.clusters[0].replica.primary. Keys
// are separated by dots; [N] indexes a list.
func parsePath(p string) ([]segment, error) {
	if p == "" {
		return nil, errors.New("the path is empty")
	}
	var segs []segment
	for i := 0; i < len(p); {
		if p[i] == '[' {
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("path %q: [ without ]", p)
			}
			digits := p[i+1 : i+j]
			if digits == "" || strings.Trim(digits, "0123456789") != "" {
				return nil, fmt.Errorf("path %q: [%s] is not a list index", p, digits)
			}
			n, err := strconv.Atoi(digits)
			if err != nil {
				return nil, fmt.Errorf("path %q: [%s] is not a list index", p, digits)
			}
			segs = append(segs, segment{index: n})
			i += j + 1
		} else {
			j := strings.IndexAny(p[i:], ".[]")
			if j < 0 {
				j = len(p) - i
			}
			if j == 0 {
				return nil, fmt.Errorf("path %q: empty key at offset %d", p, i)
			}
			segs = append(segs, segment{key: p[i : i+j], index: -1})
			i += j
		}
		if i == len(p) {
			break
		}
		switch p[i] {
		case '.':
			i++
			if i == len(p) || p[i] == '.' || p[i] == '[' || p[i] == ']' {
				return nil, fmt.Errorf("path %q: empty key at offset %d", p, i)
			}
		case '[':
		default:
			return nil, fmt.Errorf("path %q: unexpected %q at offset %d", p, p[i], i)
		}
	}
	return segs, nil
}

// documents decodes every document of a YAML stream. The index of a
// document counts every document of the stream, empty ones included.
func documents(data []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []*yaml.Node
	for {
		n := &yaml.Node{}
		err := dec.Decode(n)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, n)
	}
}

// emptyDocument reports whether a document holds nothing, as the one a
// final --- opens.
func emptyDocument(doc *yaml.Node) bool {
	if len(doc.Content) == 0 {
		return true
	}
	c := doc.Content[0]
	return c.Kind == yaml.ScalarNode && c.Tag == "!!null" && c.Value == "" && c.Anchor == ""
}

// selectDocument returns the index of the document to edit. Without an
// index, the stream must hold exactly one document that is not empty.
func selectDocument(docs []*yaml.Node, index *int32) (int, error) {
	if index != nil {
		i := int(*index)
		if i < 0 || i >= len(docs) {
			return 0, fmt.Errorf("document %d does not exist: the file holds %d", i, len(docs))
		}
		return i, nil
	}
	found := -1
	for i, d := range docs {
		if emptyDocument(d) {
			continue
		}
		if found >= 0 {
			return 0, errors.New("the file holds several YAML documents: the change needs a document index")
		}
		found = i
	}
	if found < 0 {
		return 0, errors.New("the file holds no YAML document")
	}
	return found, nil
}

var bom = []byte("\xef\xbb\xbf")

// text indexes the lines of a file. Lines and columns count from 1, and
// columns count characters, as yaml.v3 reports them.
type text struct {
	data  []byte
	start []int
	bom   int
}

func newText(data []byte) *text {
	t := &text{data: data, start: []int{0}}
	if bytes.HasPrefix(data, bom) {
		t.bom = len(bom)
	}
	for i, b := range data {
		if b == '\n' {
			t.start = append(t.start, i+1)
		}
	}
	return t
}

func (t *text) lines() int { return len(t.start) }

// lineStart returns the offset where line n starts, after a byte order mark.
func (t *text) lineStart(n int) int {
	if n == 1 {
		return t.bom
	}
	return t.start[n-1]
}

// line returns line n without its line break.
func (t *text) line(n int) []byte {
	if n < 1 || n > len(t.start) {
		return nil
	}
	end := len(t.data)
	if n < len(t.start) {
		end = t.start[n] - 1
	}
	return bytes.TrimSuffix(t.data[t.lineStart(n):end], []byte("\r"))
}

// next returns the offset where the line after line n starts, or the end
// of the data.
func (t *text) next(n int) int {
	if n < len(t.start) {
		return t.start[n]
	}
	return len(t.data)
}

// offset returns the byte offset of column col of line n.
func (t *text) offset(n, col int) (int, error) {
	if n < 1 || n > len(t.start) || col < 1 {
		return 0, fmt.Errorf("position %d:%d is outside the file", n, col)
	}
	l := t.line(n)
	off := 0
	for c := 1; c < col; c++ {
		if off >= len(l) {
			return 0, fmt.Errorf("position %d:%d is outside the line", n, col)
		}
		_, size := utf8.DecodeRune(l[off:])
		off += size
	}
	return t.lineStart(n) + off, nil
}

// indent counts the spaces that start line n.
func (t *text) indent(n int) int {
	l := t.line(n)
	i := 0
	for i < len(l) && l[i] == ' ' {
		i++
	}
	return i
}

// blank reports whether line n holds only spaces or tabs.
func (t *text) blank(n int) bool {
	return len(bytes.Trim(t.line(n), " \t")) == 0
}

// newline returns the line break the file uses.
func (t *text) newline() string {
	if i := bytes.IndexByte(t.data, '\n'); i > 0 && t.data[i-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// listItem reports whether a line, from its indentation on, starts a list item.
func listItem(l []byte) bool {
	return len(l) > 0 && l[0] == '-' && (len(l) == 1 || l[1] == ' ' || l[1] == '\t')
}

// entryEnd returns the offset after the last line of the block mapping
// entry whose key is k: the key's line, and every following line that is
// indented deeper, or that is an item of a list at the key's own column.
// Blank lines and comments after the entry stay outside it.
func (t *text) entryEnd(k, v *yaml.Node) int {
	col := k.Column - 1
	seq := v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0 && v.Column == k.Column
	last := k.Line
	for n := k.Line + 1; n <= t.lines(); n++ {
		if t.blank(n) {
			continue
		}
		ind := t.indent(n)
		if ind > col || (seq && ind == col && listItem(t.line(n)[ind:])) {
			last = n
			continue
		}
		break
	}
	return t.next(last)
}

// scalarSpan returns the bytes of a scalar that sits on one line. key is
// true for a mapping key.
func (t *text) scalarSpan(n *yaml.Node, name string, key bool) (int, int, error) {
	switch {
	case n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return 0, 0, fmt.Errorf("%s is a block scalar (| or >): Tack replaces only a scalar on one line", name)
	case n.Style&yaml.TaggedStyle != 0:
		return 0, 0, fmt.Errorf("%s has an explicit tag (%s): Tack replaces only an untagged scalar", name, n.Tag)
	}
	start, err := t.offset(n.Line, n.Column)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", name, err)
	}
	l := t.line(n.Line)
	rest := l[start-t.lineStart(n.Line):]
	multi := fmt.Errorf("%s spans several lines: Tack replaces only a scalar on one line", name)
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		if len(rest) == 0 || rest[0] != '"' {
			return 0, 0, fmt.Errorf("%s: no opening quote at line %d", name, n.Line)
		}
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case '\\':
				i++
			case '"':
				return start, start + i + 1, nil
			}
		}
		return 0, 0, multi
	case n.Style&yaml.SingleQuotedStyle != 0:
		if len(rest) == 0 || rest[0] != '\'' {
			return 0, 0, fmt.Errorf("%s: no opening quote at line %d", name, n.Line)
		}
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\'' {
				if i+1 < len(rest) && rest[i+1] == '\'' {
					i++
					continue
				}
				return start, start + i + 1, nil
			}
		}
		return 0, 0, multi
	}
	// A plain scalar ends at a comment, and a plain key at its colon.
	end := len(rest)
	for i := 1; i < len(rest); i++ {
		if rest[i] == '#' && (rest[i-1] == ' ' || rest[i-1] == '\t') {
			end = i
			break
		}
		if key && rest[i] == ':' && (i+1 == len(rest) || rest[i+1] == ' ' || rest[i+1] == '\t') {
			end = i
			break
		}
	}
	raw := bytes.TrimRight(rest[:end], " \t")
	if string(raw) != n.Value {
		return 0, 0, multi
	}
	return start, start + len(raw), nil
}

// lookup returns the key and value of key in a mapping.
func lookup(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key {
			return k, m.Content[i+1]
		}
	}
	return nil, nil
}

func hasMergeKey(m *yaml.Node) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Tag == "!!merge" {
			return true
		}
	}
	return false
}

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }

func describeNode(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.AliasNode:
		return "an alias"
	case yaml.ScalarNode:
		if isNull(n) {
			return "empty (null)"
		}
		return fmt.Sprintf("the scalar %q", n.Value)
	}
	return "an unknown node"
}

// step is a node on the path, with its key when it is a mapping value.
type step struct {
	node *yaml.Node
	key  *yaml.Node
}

// walk follows segs from root. It returns the root and one step per
// segment that exists; a missing key or list item ends the walk early.
func walk(root *yaml.Node, segs []segment) ([]step, error) {
	chain := []step{{node: root}}
	cur := root
	for i, s := range segs {
		n := cur
		if n.Kind == yaml.AliasNode && n.Alias != nil {
			n = n.Alias
		}
		if isNull(n) {
			return chain, nil
		}
		var next step
		if s.index < 0 {
			if n.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("%s is %s, not a mapping", where(segs[:i]), describeNode(n))
			}
			k, v := lookup(n, s.key)
			if k == nil {
				if hasMergeKey(n) {
					return nil, fmt.Errorf("%s has a merge key (<<): Tack cannot tell whether %s is set through it", where(segs[:i]), s.key)
				}
				return chain, nil
			}
			next = step{node: v, key: k}
		} else {
			if n.Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("%s is %s, not a list", where(segs[:i]), describeNode(n))
			}
			if s.index >= len(n.Content) {
				return chain, nil
			}
			next = step{node: n.Content[s.index]}
		}
		chain = append(chain, next)
		cur = next.node
	}
	return chain, nil
}

// editable checks that the nodes of chain can be edited in place: block
// style, no anchor and no alias.
func editable(chain []step, segs []segment) error {
	for i, st := range chain {
		n, name := st.node, where(segs[:i])
		switch {
		case n.Kind == yaml.AliasNode:
			return fmt.Errorf("%s is an alias (*%s): Tack does not edit through aliases", name, n.Value)
		case n.Anchor != "":
			return fmt.Errorf("%s has an anchor (&%s): other nodes may alias it", name, n.Anchor)
		case (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && n.Style&yaml.FlowStyle != 0:
			return fmt.Errorf("%s is in flow style ({} or []): Tack edits only block style", name)
		}
	}
	return nil
}

// splice replaces the bytes from start to end with text.
type splice struct {
	start, end int
	text       string
}

// editRequest is one field to set or remove.
type editRequest struct {
	segs   []segment
	remove bool
	// text is the value as YAML, and want the value yaml.v3 reads from it.
	text string
	want any
	// strict requires the parent of the field to exist.
	strict bool
}

// standIn is a value that never equals a value in a file. checkField uses it.
type standIn struct{}

// planEdit finds the bytes to change in the selected document. It returns
// a nil splice when nothing needs to change, and the document's index.
func planEdit(data []byte, document *int32, req editRequest) (*splice, int, error) {
	docs, err := documents(data)
	if err != nil {
		return nil, 0, fmt.Errorf("the file is not valid YAML: %w", err)
	}
	di, err := selectDocument(docs, document)
	if err != nil {
		return nil, 0, err
	}
	doc := docs[di]
	if len(doc.Content) == 0 {
		return nil, di, fmt.Errorf("document %d is empty", di)
	}
	segs := req.segs
	chain, err := walk(doc.Content[0], segs)
	if err != nil {
		return nil, di, err
	}
	t := newText(data)
	depth := len(chain) - 1
	switch {
	case depth == len(segs) && req.remove:
		sp, err := planRemove(t, chain, segs)
		return sp, di, err
	case depth == len(segs):
		sp, err := planReplace(t, chain, segs, req)
		return sp, di, err
	case req.remove:
		return nil, di, nil
	case req.strict && depth < len(segs)-1:
		return nil, di, fmt.Errorf("%s does not exist", formatPath(segs[:depth+1]))
	}
	sp, err := planInsert(t, chain, segs, req)
	return sp, di, err
}

func planReplace(t *text, chain []step, segs []segment, req editRequest) (*splice, error) {
	if err := editable(chain, segs); err != nil {
		return nil, err
	}
	leaf := chain[len(chain)-1]
	n, name := leaf.node, formatPath(segs)
	if n.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("%s is %s: Tack writes only a scalar", name, describeNode(n))
	}
	var cur any
	if err := n.Decode(&cur); err == nil && reflect.DeepEqual(cur, req.want) {
		return nil, nil
	}
	if isNull(n) && n.Value == "" && n.Style == 0 {
		if leaf.key == nil {
			return nil, fmt.Errorf("%s is an empty list item: Tack cannot write it", name)
		}
		_, end, err := t.scalarSpan(leaf.key, "the key of "+name, true)
		if err != nil {
			return nil, err
		}
		rest := t.data[end:t.next(leaf.key.Line)]
		i := 0
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
			i++
		}
		if i == len(rest) || rest[i] != ':' {
			return nil, fmt.Errorf("%s: no colon after the key at line %d", name, leaf.key.Line)
		}
		pos := end + i + 1
		return &splice{start: pos, end: pos, text: " " + req.text}, nil
	}
	start, end, err := t.scalarSpan(n, name, false)
	if err != nil {
		return nil, err
	}
	return &splice{start: start, end: end, text: req.text}, nil
}

func planRemove(t *text, chain []step, segs []segment) (*splice, error) {
	if err := editable(chain, segs); err != nil {
		return nil, err
	}
	name := formatPath(segs)
	if segs[len(segs)-1].index >= 0 {
		return nil, fmt.Errorf("%s is a list item: Tack removes only a mapping key", name)
	}
	leaf, parent := chain[len(chain)-1], chain[len(chain)-2].node
	if len(parent.Content) == 2 {
		return nil, fmt.Errorf("%s is the only key of %s: removing it would leave %s empty", name, where(segs[:len(segs)-1]), where(segs[:len(segs)-1]))
	}
	k := leaf.key
	ks, err := t.offset(k.Line, k.Column)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	ls := t.lineStart(k.Line)
	if len(bytes.TrimLeft(t.data[ls:ks], " ")) > 0 {
		return nil, fmt.Errorf("%s shares its line with other content, such as a list item marker: Tack cannot remove it", name)
	}
	return &splice{start: ls, end: t.entryEnd(k, leaf.node)}, nil
}

func planInsert(t *text, chain []step, segs []segment, req editRequest) (*splice, error) {
	if err := editable(chain, segs); err != nil {
		return nil, err
	}
	depth := len(chain) - 1
	missing := segs[depth:]
	for i, s := range missing {
		if s.index >= 0 {
			return nil, fmt.Errorf("%s does not exist: Tack does not create list items", formatPath(segs[:depth+i+1]))
		}
	}
	c := chain[depth].node
	if c.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s is %s: Tack inserts %s only into a block mapping that has entries", where(segs[:depth]), describeNode(c), missing[0].key)
	}
	first := c.Content[0]
	lastK, lastV := c.Content[len(c.Content)-2], c.Content[len(c.Content)-1]
	pad := first.Column - 1
	indentStep := 2
	if k := chain[depth].key; k != nil && first.Column > k.Column {
		indentStep = first.Column - k.Column
	}
	pos := t.entryEnd(lastK, lastV)
	nl := t.newline()
	var b strings.Builder
	if pos == len(t.data) && pos > 0 && t.data[pos-1] != '\n' {
		b.WriteString(nl)
	}
	for i, s := range missing {
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString(renderString(s.key))
		b.WriteByte(':')
		if i == len(missing)-1 {
			b.WriteString(" " + req.text)
		}
		b.WriteString(nl)
		pad += indentStep
	}
	return &splice{start: pos, end: pos, text: b.String()}, nil
}

// renderJSON reads a JSON scalar and writes it as a YAML scalar. It
// returns the YAML text and the value yaml.v3 reads from it.
func renderJSON(js string) (string, any, error) {
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", nil, fmt.Errorf("value %s is not JSON: %w", js, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", nil, fmt.Errorf("value %s is not one JSON value", js)
	}
	switch x := v.(type) {
	case nil:
		return "null", nil, nil
	case bool:
		return strconv.FormatBool(x), x, nil
	case json.Number:
		if i, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return x.String(), int(i), nil
		}
		f, err := strconv.ParseFloat(x.String(), 64)
		if err != nil {
			return "", nil, fmt.Errorf("value %s: %w", js, err)
		}
		return x.String(), f, nil
	case string:
		return renderString(x), x, nil
	}
	return "", nil, fmt.Errorf("value %s is not a string, number, boolean or null: Tack writes only scalars", js)
}

// yaml11 holds the plain words that YAML 1.1 readers, such as Helm's,
// take for booleans or null.
var yaml11 = map[string]bool{
	"y": true, "yes": true, "n": true, "no": true, "true": true, "false": true,
	"on": true, "off": true, "null": true,
}

// plainSafe reports whether s can be written without quotes and still read
// back as the same string by YAML 1.1 and 1.2 readers.
func plainSafe(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '/':
		case i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	if yaml11[strings.ToLower(s)] {
		return false
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(s), &n); err != nil || len(n.Content) != 1 {
		return false
	}
	c := n.Content[0]
	return c.Kind == yaml.ScalarNode && c.Tag == "!!str" && c.Value == s
}

// renderString writes s plain when that is safe, else double-quoted.
func renderString(s string) string {
	if plainSafe(s) {
		return s
	}
	return doubleQuote(s)
}

func doubleQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0x85:
			b.WriteString(`\N`)
		case 0x2028:
			b.WriteString(`\L`)
		case 0x2029:
			b.WriteString(`\P`)
		default:
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0xfeff {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// setField sets the field at path in the selected document of a YAML file
// and returns the new bytes. value is JSON; nil removes the field, and
// removing a field that is absent changes nothing. Only the bytes of the
// field change: comments, order and quoting elsewhere stay as they are.
func setField(data []byte, document *int32, path string, value *string) ([]byte, error) {
	segs, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	req := editRequest{segs: segs, remove: value == nil}
	if value != nil {
		if req.text, req.want, err = renderJSON(*value); err != nil {
			return nil, err
		}
	}
	sp, di, err := planEdit(data, document, req)
	if err != nil || sp == nil {
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	out := make([]byte, 0, len(data)+len(sp.text))
	out = append(out, data[:sp.start]...)
	out = append(out, sp.text...)
	out = append(out, data[sp.end:]...)
	if err := verify(data, out, di, segs, req.remove, req.want); err != nil {
		return nil, err
	}
	return out, nil
}

// checkField checks that setField can edit path, without a value. With
// parent set, the parent of the field must exist, unless the field is
// removed.
func checkField(data []byte, document *int32, path string, remove, parent bool) error {
	segs, err := parsePath(path)
	if err != nil {
		return err
	}
	_, _, err = planEdit(data, document, editRequest{segs: segs, remove: remove, text: `"x"`, want: standIn{}, strict: parent})
	return err
}

// verify checks that out holds what data holds, with only the change applied.
func verify(data, out []byte, doc int, segs []segment, remove bool, want any) error {
	failed := func(why string) error {
		return fmt.Errorf("the edit of %s %s: Tack left the file as it was", formatPath(segs), why)
	}
	before, err := decodeAll(data)
	if err != nil {
		return err
	}
	after, err := decodeAll(out)
	if err != nil {
		return failed("made the file invalid (" + err.Error() + ")")
	}
	if len(before) != len(after) {
		return failed("changed the number of documents")
	}
	for i := range before {
		exp := before[i]
		if i == doc {
			if exp, err = applyValue(exp, segs, remove, want); err != nil {
				return failed(err.Error())
			}
		}
		if !reflect.DeepEqual(exp, after[i]) {
			return failed("changed more than the field")
		}
	}
	return nil
}

func decodeAll(data []byte) ([]any, error) {
	docs, err := documents(data)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(docs))
	for i, d := range docs {
		if err := d.Decode(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// applyValue sets or removes the value at segs in a decoded document,
// creating missing mappings.
func applyValue(tree any, segs []segment, remove bool, want any) (any, error) {
	if len(segs) == 0 {
		return want, nil
	}
	s, last := segs[0], len(segs) == 1
	if s.index >= 0 {
		l, ok := tree.([]any)
		if !ok || s.index >= len(l) {
			return nil, fmt.Errorf("has no list item %d", s.index)
		}
		v, err := applyValue(l[s.index], segs[1:], remove, want)
		l[s.index] = v
		return l, err
	}
	switch m := tree.(type) {
	case map[string]any:
		if last && remove {
			delete(m, s.key)
			return m, nil
		}
		child, ok := m[s.key]
		if !ok && !last {
			child = map[string]any{}
		}
		v, err := applyValue(child, segs[1:], remove, want)
		m[s.key] = v
		return m, err
	case map[any]any:
		if last && remove {
			delete(m, s.key)
			return m, nil
		}
		child, ok := m[s.key]
		if !ok && !last {
			child = map[string]any{}
		}
		v, err := applyValue(child, segs[1:], remove, want)
		m[s.key] = v
		return m, err
	}
	return nil, fmt.Errorf("found no mapping for %s", s.key)
}
