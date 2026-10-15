package config

import (
	"bytes"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// secretKey names a value that is a credential. A path to one (_file, _path)
// or an endpoint (_url) is not.
var secretKey = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[-_]?key|authorization|cookie|credential|private[-_]?key)$`)

// Redact returns a check document with its credentials masked: values under
// a secret-looking key, and the password of any URL. A ${VAR} reference is
// kept — it names a secret without being one.
func Redact(doc []byte) []byte {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// An accepted document always parses; anything else shows nothing.
			return []byte("# redacted: the document could not be parsed\n")
		}
		redactNode(&n, false)
		if err := enc.Encode(&n); err != nil {
			return []byte("# redacted: the document could not be written\n")
		}
	}
	_ = enc.Close()
	return out.Bytes()
}

func redactNode(n *yaml.Node, secret bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		switch {
		case secret && !onlyReferences(n.Value):
			n.Value, n.Style, n.Tag = "<redacted>", 0, "!!str"
		case strings.Contains(n.Value, "@"):
			n.Value = RedactURL(n.Value)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			redactNode(n.Content[i+1], secret || secretKey.MatchString(n.Content[i].Value))
		}
	default:
		for _, c := range n.Content {
			redactNode(c, secret)
		}
	}
}

func onlyReferences(v string) bool {
	return strings.TrimSpace(envRef.ReplaceAllString(v, "")) == "" && v != ""
}

// RedactURL masks the password of a URL and returns anything else unchanged:
// a target's name is a label on pages nobody signs in to.
func RedactURL(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, set := u.User.Password(); !set {
		return s
	}
	return u.Redacted()
}
