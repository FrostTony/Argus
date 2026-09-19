package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration reads "30s" or a bare number of seconds.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var raw any
	if err := n.Decode(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("line %d: invalid duration %q: %w", n.Line, v, err)
		}
		*d = Duration(parsed)
	case int:
		*d = Duration(time.Duration(v) * time.Second)
	case float64:
		*d = Duration(v * float64(time.Second))
	default:
		return fmt.Errorf("line %d: duration must be a string (\"30s\") or a number of seconds", n.Line)
	}
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
