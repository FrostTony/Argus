package scenario

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// A reference is ${var.NAME}, ${secret.NAME} or ${run.id}. ${NAME}, an
// environment variable, was replaced when the file was read; one that is still
// here was not set, and is reported as such rather than sent as text.
var (
	refPattern = regexp.MustCompile(`\$\{([^{}]*)\}`)
	identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const (
	nsVar    = "var"
	nsSecret = "secret"
	nsRun    = "run"
)

type ref struct{ ns, name string }

func (r ref) String() string { return "${" + r.ns + "." + r.name + "}" }

// template is a value split into text and references once, when the scenario
// is built, so a run only fills it in.
type template struct {
	text []string // len(refs)+1 pieces of text around the references
	refs []ref
	// spans are where each reference sits in the value as written.
	spans [][2]int
}

func parseTemplate(s string) (template, error) {
	var t template
	last := 0
	for _, m := range refPattern.FindAllStringSubmatchIndex(s, -1) {
		inner := s[m[2]:m[3]]
		ns, name, _ := strings.Cut(inner, ".")
		switch {
		case ns != nsVar && ns != nsSecret && ns != nsRun:
			if identifier.MatchString(inner) {
				return t, fmt.Errorf("${%s}: the environment variable is not set", inner)
			}
			return t, fmt.Errorf("${%s}: want ${var.NAME}, ${secret.NAME} or ${run.id}", inner)
		case !identifier.MatchString(name):
			return t, fmt.Errorf("${%s}: %q is not a name", inner, name)
		case ns == nsRun && name != "id":
			return t, fmt.Errorf("${%s}: a run knows only ${run.id}", inner)
		}
		t.text = append(t.text, s[last:m[0]])
		t.refs = append(t.refs, ref{ns, name})
		t.spans = append(t.spans, [2]int{m[0], m[1]})
		last = m[1]
	}
	t.text = append(t.text, s[last:])
	return t, nil
}

// whole reports whether the value is one reference and nothing else.
func (t template) whole() bool {
	return len(t.refs) == 1 && t.text[0] == "" && t.text[1] == ""
}

// errUnset is a reference to a variable no step has set yet, which only a
// cleanup step can meet: everywhere else it is refused when the file is read.
type errUnset struct{ ref ref }

func (e errUnset) Error() string { return e.ref.String() + " is not set" }

// escaper prepares a value for where it is put, or refuses it there.
type escaper func(string) (string, error)

// render fills in the references; escape, when given, is applied to each value
// and never to the text around it.
func (t template) render(s *scope, escape escaper) (string, error) {
	if len(t.refs) == 0 {
		return t.text[0], nil
	}
	var b strings.Builder
	for i, r := range t.refs {
		v, ok := s.lookup(r)
		if !ok {
			return "", errUnset{r}
		}
		if escape != nil {
			var err error
			if v, err = escape(v); err != nil {
				return "", fmt.Errorf("%s: %w", r, err)
			}
		}
		b.WriteString(t.text[i])
		b.WriteString(v)
	}
	b.WriteString(t.text[len(t.text)-1])
	return b.String(), nil
}

// segment puts a value into a path as one segment of it, or refuses it.
// Escaping alone does not do that: "." and ".." mean the same thing however
// they are spelled — a server decodes %2E%2E and cleans the path — and an
// empty value joins its neighbours. "/videos/${var.id}/files" with an id of
// ".." would DELETE /files.
func segment(v string) (string, error) {
	switch v {
	case "":
		return "", errors.New("an empty value cannot be a path segment")
	case ".", "..":
		return "", fmt.Errorf("%q cannot be a path segment", v)
	}
	// A colon is left alone by PathEscape, and in the first segment of a
	// relative path it reads as a scheme: "urn:42/files" leaves the target.
	return strings.ReplaceAll(url.PathEscape(v), ":", "%3A"), nil
}

// inPath checks that every reference of an address sits in its path: a value
// in the host would choose the server, and one in the query would be read as
// parameters of its own. Values for the query go under query:, which encodes
// them.
func (t template) inPath(raw string, absolute bool) error {
	start := 0
	if absolute {
		if i := strings.Index(raw, "://"); i >= 0 {
			rest := raw[i+3:]
			if j := strings.IndexAny(rest, "/?#"); j >= 0 {
				start = i + 3 + j
			} else {
				start = len(raw)
			}
		}
	}
	end := len(raw)
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		end = i
	}
	for i, sp := range t.spans {
		switch {
		case sp[0] < start:
			return fmt.Errorf("%s: a reference may stand for the whole address or sit in its path, not in its host", t.refs[i])
		case sp[1] > end:
			return fmt.Errorf("%s: a value for the query goes under query:, which encodes it", t.refs[i])
		}
	}
	return nil
}

// scope is what a run's references resolve to.
type scope struct {
	vars    map[string]string
	secrets map[string]string
	runID   string
	// masks are the secrets as they may appear in a message, longest first.
	masks []mask
}

type mask struct{ text, ref string }

func newScope(vars, secrets map[string]string) *scope {
	s := &scope{vars: vars, secrets: secrets, runID: newRunID()}
	for name, v := range secrets {
		ref := ref{nsSecret, name}.String()
		quoted, _ := json.Marshal(v)
		for _, form := range []string{v, url.QueryEscape(v), url.PathEscape(v), string(quoted[1 : len(quoted)-1])} {
			s.masks = append(s.masks, mask{form, ref})
		}
	}
	// Longest first: a secret that contains another must not be left with
	// the other masked and the rest of it in the clear.
	slices.SortFunc(s.masks, func(a, b mask) int { return cmp.Compare(len(b.text), len(a.text)) })
	return s
}

func (s *scope) lookup(r ref) (string, bool) {
	switch r.ns {
	case nsVar:
		v, ok := s.vars[r.name]
		return v, ok
	case nsSecret:
		v, ok := s.secrets[r.name]
		return v, ok
	case nsRun:
		return s.runID, true
	}
	return "", false
}

// redact replaces every secret in a message, as sent and as escaped for a
// URL or a JSON string: an error quotes what the target answered, and a
// target may well echo what it was sent.
func (s *scope) redact(msg string) string {
	if s == nil {
		return msg
	}
	for _, m := range s.masks {
		msg = strings.ReplaceAll(msg, m.text, m.ref)
	}
	return msg
}

// definitions tracks which variables exist at a point of the scenario, so a
// reference to one that does not is refused when the file is read.
type definitions struct {
	vars    map[string]bool
	secrets map[string]bool
}

func (d definitions) check(t template) error {
	for _, r := range t.refs {
		switch {
		case r.ns == nsVar && !d.vars[r.name]:
			return fmt.Errorf("%s is not set by vars or by an earlier step's extract", r)
		case r.ns == nsSecret && !d.secrets[r.name]:
			return fmt.Errorf("%s is not declared under secrets", r)
		}
	}
	return nil
}
