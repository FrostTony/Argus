package metrics

import (
	"slices"
	"strings"
)

type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Labels is sorted by name, free of duplicates, and immutable: every operation
// returns a new slice.
type Labels []Label

// L builds a set from key-value pairs. An odd trailing element is dropped.
func L(kv ...string) Labels {
	out := make(Labels, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, Label{kv[i], kv[i+1]})
	}
	return normalize(out)
}

// With returns a copy with the label added or replaced; "" removes it.
func (l Labels) With(name, value string) Labels {
	i, found := slices.BinarySearchFunc(l, name, func(lb Label, n string) int {
		return strings.Compare(lb.Name, n)
	})
	if value == "" {
		if !found {
			return l
		}
		out := make(Labels, 0, len(l)-1)
		out = append(out, l[:i]...)
		return append(out, l[i+1:]...)
	}
	if found {
		out := make(Labels, len(l))
		copy(out, l)
		out[i].Value = value
		return out
	}
	out := make(Labels, 0, len(l)+1)
	out = append(out, l[:i]...)
	out = append(out, Label{name, value})
	return append(out, l[i:]...)
}

// Merge overlays other on l; same-named labels from other win.
func (l Labels) Merge(other Labels) Labels {
	switch {
	case len(other) == 0:
		return l
	case len(l) == 0:
		return slices.Clone(other)
	case len(other) == 1:
		return l.With(other[0].Name, other[0].Value)
	}
	out := make(Labels, 0, len(l)+len(other))
	out = append(out, other...)
	for _, lb := range l {
		if other.Get(lb.Name) == "" {
			out = append(out, lb)
		}
	}
	return normalize(out)
}

func (l Labels) Get(name string) string {
	for _, lb := range l {
		if lb.Name == name {
			return lb.Value
		}
	}
	return ""
}

// Key is a stable representation suitable as a map key.
func (l Labels) Key() string {
	if len(l) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(l.keyLen())
	for i, lb := range l {
		if i > 0 {
			b.WriteByte(keySep)
		}
		b.WriteString(lb.Name)
		b.WriteByte(keyEq)
		b.WriteString(lb.Value)
	}
	return b.String()
}

const (
	keySep = 0x1f
	keyEq  = 0x1e
)

func (l Labels) keyLen() int {
	n := 2*len(l) - 1
	for _, lb := range l {
		n += len(lb.Name) + len(lb.Value)
	}
	return n
}

// AppendKey writes Key into buf, for lookups that must not allocate.
func (l Labels) AppendKey(buf []byte) []byte {
	for i, lb := range l {
		if i > 0 {
			buf = append(buf, keySep)
		}
		buf = append(buf, lb.Name...)
		buf = append(buf, keyEq)
		buf = append(buf, lb.Value...)
	}
	return buf
}

// String renders Prometheus style: {a="1",b="2"}.
func (l Labels) String() string {
	if len(l) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, lb := range l {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(lb.Name)
		b.WriteString(`="`)
		b.WriteString(escapeValue(lb.Value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func (l Labels) Map() map[string]string {
	m := make(map[string]string, len(l))
	for _, lb := range l {
		m[lb.Name] = lb.Value
	}
	return m
}

// normalize sorts, keeps the last of duplicates, and drops empty values.
func normalize(l Labels) Labels {
	if len(l) > 1 && !slices.IsSortedFunc(l, byName) {
		slices.SortStableFunc(l, byName)
	}
	out := l[:0]
	for i, lb := range l {
		if i+1 < len(l) && l[i+1].Name == lb.Name {
			continue
		}
		if lb.Value == "" {
			continue
		}
		out = append(out, lb)
	}
	return out
}

func byName(a, b Label) int { return strings.Compare(a.Name, b.Name) }

var valueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeValue(s string) string { return valueEscaper.Replace(s) }
