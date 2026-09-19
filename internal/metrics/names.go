package metrics

import (
	"fmt"
	"strings"
)

func validNameByte(c byte, i int, allowColon bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		return true
	case c == ':':
		return allowColon
	case c >= '0' && c <= '9':
		return i > 0
	}
	return false
}

// ValidMetricName reports whether s is a usable metric name.
func ValidMetricName(s string) bool { return validName(s, true) }

// ValidLabelName reports whether s is a usable label name. Names starting with
// __ are reserved by Prometheus itself.
func ValidLabelName(s string) bool {
	return validName(s, false) && !strings.HasPrefix(s, "__")
}

func validName(s string, allowColon bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !validNameByte(s[i], i, allowColon) {
			return false
		}
	}
	return true
}

// SanitizeName rewrites a name into a valid one, for names built at runtime.
func SanitizeName(s string) string {
	if ValidMetricName(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if validNameByte(s[i], i, true) {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte('_')
	}
	out := b.String()
	if out == "" || !validNameByte(out[0], 0, true) {
		return "_" + out
	}
	return out
}

// ValidateLabels rejects labels that would break the exposition.
func ValidateLabels(l Labels) error {
	for _, lb := range l {
		if !ValidLabelName(lb.Name) {
			return fmt.Errorf("label name %q is not usable: want [a-zA-Z_][a-zA-Z0-9_]*", lb.Name)
		}
		if reservedLabels[lb.Name] {
			return fmt.Errorf("label name %q is reserved: the exposition adds it to buckets and info metrics, "+
				"and a series carrying it twice makes the whole page unparseable", lb.Name)
		}
		if strings.ContainsAny(lb.Value, string([]byte{keySep, keyEq})) {
			return fmt.Errorf("label %q: the value contains a byte the series key uses as a separator", lb.Name)
		}
	}
	return nil
}

// reservedLabels are the names the exposition writes itself.
var reservedLabels = map[string]bool{
	"le": true, "val": true, "quantile": true,
}
