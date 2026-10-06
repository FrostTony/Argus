package config

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadServer reads the node configuration; an empty path yields the defaults.
// It decodes on top of DefaultServer: an unset bool is not an explicit false.
func LoadServer(path string) (Server, error) {
	cfg := DefaultServer()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("server config: %w", err)
		}
		if err := decodeDocument(data, &cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", cmp.Or(path, "<defaults>"), err)
	}
	return cfg, nil
}

// LoadProbes reads files and directories of *.yaml/*.yml as one configuration.
func LoadProbes(paths ...string) (Probes, error) {
	f, err := LoadFiles(paths...)
	return f.Probes, err
}

// Files is a check configuration read from disk, with what identifies it.
type Files struct {
	Probes Probes
	// Document is the text read, ${VAR} references as written: a single file as
	// it is, several as a YAML stream with a document per file.
	Document []byte
	// Fingerprint is what Fingerprint returns for the same files.
	Fingerprint string
}

// LoadFiles is LoadProbes plus the text and the fingerprint of what it read,
// all three taken from the same read of each file.
func LoadFiles(paths ...string) (Files, error) {
	var out Files
	files, err := expandPaths(paths)
	if err != nil {
		return out, err
	}
	if len(files) == 0 {
		return out, fmt.Errorf("check config: no files found")
	}
	sum := sha256.New()
	texts := make([][]byte, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return out, fmt.Errorf("check config: %w", err)
		}
		stamp(sum, f, data)
		texts = append(texts, data)

		var p Probes
		if err := decodeDocument(data, &p); err != nil {
			return out, fmt.Errorf("%s: %w", f, err)
		}
		if err := out.Probes.absorb(p, f); err != nil {
			return out, err
		}
	}
	out.Document = document(files, texts)
	out.Fingerprint = hex.EncodeToString(sum.Sum(nil))
	return out, nil
}

// document joins files into one text: a single file stays as it is, several
// become a YAML stream with a document per file, headed by its name.
func document(names []string, texts [][]byte) []byte {
	if len(texts) == 1 {
		return texts[0]
	}
	var b bytes.Buffer
	for i, text := range texts {
		fmt.Fprintf(&b, "--- # %s\n%s", names[i], text)
		if !bytes.HasSuffix(text, []byte("\n")) {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// absorb folds one file into the whole: a later file's defaults override an
// earlier file's, while redefining a template is rejected.
func (p *Probes) absorb(other Probes, src string) error {
	p.Defaults = p.Defaults.mergeFrom(other.Defaults)
	if len(other.Templates) > 0 && p.Templates == nil {
		p.Templates = make(map[string]Probe, len(other.Templates))
	}
	for name, tpl := range other.Templates {
		if _, dup := p.Templates[name]; dup {
			return fmt.Errorf("%s: template %q is already defined in another file", src, name)
		}
		p.Templates[name] = tpl
	}
	p.List = append(p.List, other.List...)
	return nil
}

// SaveProbes writes a check configuration back to its file byte for byte, through
// a temporary file renamed into place; it keeps the file's mode and its symlink.
func SaveProbes(path string, data []byte) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	// A check configuration carries tokens, so a new file is private.
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".argus-probes-*")
	if err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename below succeeded
	write := func() error {
		if _, err := tmp.Write(data); err != nil {
			return err
		}
		if err := tmp.Chmod(mode); err != nil {
			return err
		}
		// The rename is only durable once the contents are on disk.
		return tmp.Sync()
	}
	if err := errors.Join(write(), tmp.Close()); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	return nil
}

// Hash is the sha256 of a configuration document, over the exact bytes that
// were accepted. Whoever sent them can compute the same value and tell whether
// the node is already running that configuration.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Fingerprint hashes the check configuration on disk, file names and content both.
func Fingerprint(paths ...string) (string, error) {
	files, err := expandPaths(paths)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("check config: %w", err)
		}
		stamp(sum, f, data)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// stamp adds one file to a fingerprint: its name, its length and its content.
func stamp(w io.Writer, name string, data []byte) {
	fmt.Fprintf(w, "%s\x00%d\x00", name, len(data))
	w.Write(data)
}

func expandPaths(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("check config: %w", err)
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		// Sorted across both extensions at once: a later file's defaults win, so
		// 10-base.yml has to be read before 90-override.yaml.
		var dir []string
		for _, ext := range []string{"*.yaml", "*.yml"} {
			m, err := filepath.Glob(filepath.Join(p, ext))
			if err != nil {
				return nil, err
			}
			dir = append(dir, m...)
		}
		slices.Sort(dir)
		out = append(out, dir...)
	}
	return out, nil
}

// ParseProbes reads a whole check configuration the way the file loader does,
// ${VAR} expansion included, so the same bytes mean the same thing either way.
func ParseProbes(data []byte) (Probes, error) {
	var p Probes
	if err := decodeDocument(data, &p); err != nil {
		return p, err
	}
	if _, err := p.Resolve(); err != nil {
		return p, err
	}
	return p, nil
}

// ParseProbe reads a single probe definition, for the ad-hoc check API.
func ParseProbe(data []byte) (Probe, error) {
	var p Probe
	if err := decodeDocument(data, &p); err != nil {
		return p, err
	}
	return p, p.validate()
}

// decodeDocument reads a configuration strictly. ${VAR} is expanded inside the
// parsed scalars, so a value may hold any character without changing the
// structure around it, and error lines still point into the text as written.
func decodeDocument(data []byte, dst any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	// A second document would be silently ignored, and with it whatever it says.
	var next yaml.Node
	if err := dec.Decode(&next); err != io.EOF {
		if err != nil {
			return err
		}
		if len(next.Content) > 0 && next.Content[0].ShortTag() != "!!null" {
			return fmt.Errorf("line %d: a configuration is a single YAML document", next.Line)
		}
	}
	expandEnv(&doc)
	if err := knownFields(&doc, reflect.TypeOf(dst)); err != nil {
		return err
	}
	return doc.Decode(dst)
}

func decodeStrictBytes(data []byte, dst any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// decodeStrict re-serializes the node: yaml.Node.Decode cannot enforce KnownFields.
func decodeStrict(n *yaml.Node, dst any) error {
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return decodeStrictBytes(data, dst)
}

// maxAliasNodes bounds what aliases may expand into.
const maxAliasNodes = 1 << 20

// resolved replaces every alias with what it points at and folds in merge keys.
func resolved(n *yaml.Node) (*yaml.Node, error) {
	r := resolver{open: map[*yaml.Node]bool{}}
	return r.resolve(n, false)
}

type resolver struct {
	// open holds the anchors being expanded, to catch one that contains itself.
	open    map[*yaml.Node]bool
	aliased int
}

func (r *resolver) resolve(n *yaml.Node, viaAlias bool) (*yaml.Node, error) {
	if n.Kind == yaml.AliasNode {
		target := n.Alias
		if r.open[target] {
			return nil, fmt.Errorf("line %d: anchor %q contains itself", n.Line, n.Value)
		}
		r.open[target] = true
		defer delete(r.open, target)
		n, viaAlias = target, true
	}
	if viaAlias {
		if r.aliased++; r.aliased > maxAliasNodes {
			return nil, fmt.Errorf("line %d: aliases expand to more than %d nodes", n.Line, maxAliasNodes)
		}
	}
	out := *n
	out.Anchor = ""
	out.Content = make([]*yaml.Node, 0, len(n.Content))
	for _, c := range n.Content {
		child, err := r.resolve(c, viaAlias)
		if err != nil {
			return nil, err
		}
		out.Content = append(out.Content, child)
	}
	if out.Kind == yaml.MappingNode {
		out.Content = mergeKeys(out.Content)
	}
	return &out, nil
}

// mergeKeys folds `<<:` into the mapping around it; keys written out win.
func mergeKeys(pairs []*yaml.Node) []*yaml.Node {
	own := make(map[string]bool, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		own[pairs[i].Value] = true
	}
	var out []*yaml.Node
	for i := 0; i+1 < len(pairs); i += 2 {
		k, v := pairs[i], pairs[i+1]
		if k.Tag != "!!merge" {
			out = append(out, k, v)
			continue
		}
		sources := []*yaml.Node{v}
		if v.Kind == yaml.SequenceNode {
			sources = v.Content
		}
		for _, src := range sources {
			for j := 0; j+1 < len(src.Content); j += 2 {
				if name := src.Content[j].Value; !own[name] {
					own[name] = true
					out = append(out, src.Content[j], src.Content[j+1])
				}
			}
		}
	}
	return out
}

// envRef matches only ${VAR}: a bare $ is an anchor in the regexes configs carry.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv substitutes ${VAR} in every scalar; an unknown name is left as text.
func expandEnv(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode {
		v := envRef.ReplaceAllStringFunc(n.Value, func(ref string) string {
			if v, ok := os.LookupEnv(ref[2 : len(ref)-1]); ok {
				return v
			}
			return ref
		})
		if v != n.Value {
			n.Value = v
			// A plain scalar is typed by its text, and the text is new: 8080 is a number.
			if n.Style == 0 {
				n.Tag = ""
			}
		}
	}
	for _, c := range n.Content {
		expandEnv(c)
	}
}

// shaped is implemented by types with a YAML form of their own.
type shaped interface {
	shape(n *yaml.Node) reflect.Type
}

var (
	shapedType      = reflect.TypeFor[shaped]()
	unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()
)

// knownFields reports the first mapping key the target type has no field for.
func knownFields(n *yaml.Node, t reflect.Type) error {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	case yaml.AliasNode:
		n = n.Alias
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(shapedType) {
		t = reflect.Zero(t).Interface().(shaped).shape(n)
	} else if reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil // reads itself: a duration, a probe inside a template
	}
	switch {
	case n.Kind == yaml.SequenceNode && t.Kind() == reflect.Slice:
		for _, item := range n.Content {
			if err := knownFields(item, t.Elem()); err != nil {
				return err
			}
		}
	case n.Kind == yaml.MappingNode && t.Kind() == reflect.Map:
		for i := 1; i < len(n.Content); i += 2 {
			if err := knownFields(n.Content[i], t.Elem()); err != nil {
				return err
			}
		}
	case n.Kind == yaml.MappingNode && t.Kind() == reflect.Struct:
		fields := yamlFields(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			ft, ok := fields[key.Value]
			if !ok {
				return fmt.Errorf("line %d: field %s not found", key.Line, key.Value)
			}
			if err := knownFields(value, ft); err != nil {
				return err
			}
		}
	}
	return nil
}

// yamlFields maps the keys a struct accepts to the types behind them.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		switch {
		case !f.IsExported() || name == "-":
		case name == "":
			out[strings.ToLower(f.Name)] = f.Type
		default:
			out[name] = f.Type
		}
	}
	return out
}
