package flux

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// segment is one step of a path: a map key, or a list index.
type segment struct {
	key   string
	index int
	isIdx bool
}

// path is a field path, such as spec.containers[0].image.
type path []segment

func (p path) String() string {
	var b strings.Builder
	for i, s := range p {
		switch {
		case s.isIdx:
			fmt.Fprintf(&b, "[%d]", s.index)
		case i > 0:
			b.WriteString(".")
			b.WriteString(s.key)
		default:
			b.WriteString(s.key)
		}
	}
	return b.String()
}

func (p path) child(key string) path {
	return append(p[:len(p):len(p)], segment{key: key})
}

func (p path) item(i int) path {
	return append(p[:len(p):len(p)], segment{index: i, isIdx: true})
}

// overlaps is true when one path is a prefix of the other.
func overlaps(a, b path) bool {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// parsePath parses a dotted field path. [N] indexes a list.
func parsePath(s string) (path, error) {
	var p path
	if s == "" {
		return nil, fmt.Errorf("empty field path")
	}
	for _, part := range strings.Split(s, ".") {
		name := part
		var idx []int
		if i := strings.IndexByte(part, '['); i >= 0 {
			name = part[:i]
			rest := part[i:]
			for rest != "" {
				end := strings.IndexByte(rest, ']')
				if rest[0] != '[' || end < 0 {
					return nil, fmt.Errorf("field path %q: bad index in %q", s, part)
				}
				n, err := strconv.Atoi(rest[1:end])
				if err != nil || n < 0 {
					return nil, fmt.Errorf("field path %q: bad index in %q", s, part)
				}
				idx = append(idx, n)
				rest = rest[end+1:]
			}
		}
		if name == "" {
			return nil, fmt.Errorf("field path %q: empty segment", s)
		}
		p = append(p, segment{key: name})
		for _, n := range idx {
			p = append(p, segment{index: n, isIdx: true})
		}
	}
	return p, nil
}

// lookup returns the value at p, and whether it is there.
func lookup(obj map[string]any, p path) (any, bool) {
	var cur any = obj
	for _, s := range p {
		if s.isIdx {
			l, ok := cur.([]any)
			if !ok || s.index >= len(l) {
				return nil, false
			}
			cur = l[s.index]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[s.key]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// jsonEqual compares two values by their JSON form, so 2 and 2.0 are equal.
func jsonEqual(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// diffPaths lists the paths where a and b differ. A list that changes length
// is one path.
func diffPaths(a, b any, at path, out *[]path) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			av, ain := am[k]
			bv, bin := bm[k]
			if ain != bin {
				if !empty(av) || !empty(bv) {
					*out = append(*out, at.child(k))
				}
				continue
			}
			diffPaths(av, bv, at.child(k), out)
		}
		return
	}
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok && bok && len(al) == len(bl) {
		for i := range al {
			diffPaths(al[i], bl[i], at.item(i), out)
		}
		return
	}
	if !jsonEqual(a, b) && (!empty(a) || !empty(b)) {
		*out = append(*out, at)
	}
}

// empty is true for nil and for an empty map or list. Flux's drift check
// treats them as equal.
func empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// show formats a value for a message, cut to a readable length.
func show(v any, ok bool) string {
	if !ok {
		return "(unset)"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	const limit = 120
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}
