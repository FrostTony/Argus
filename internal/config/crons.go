package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Crons is what a probe's cron: says, written either as one specification or
// as a list of them: "0 9 * * *", or ["15 9 * * *", "40 18 * * *"] for times
// one crontab line cannot name together.
type Crons []string

// UnmarshalYAML takes a string or a list of strings; an empty one is no cron.
func (c *Crons) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*c = nil
		if strings.TrimSpace(s) != "" {
			*c = Crons{s}
		}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := n.Decode(&list); err != nil {
			return fmt.Errorf("line %d: cron is a specification or a list of them", n.Line)
		}
		out := Crons{}
		for _, s := range list {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("line %d: an empty cron specification", n.Line)
			}
			out = append(out, s)
		}
		*c = out
		return nil
	}
	return fmt.Errorf("line %d: cron is a specification or a list of them", n.Line)
}

// MarshalYAML writes one specification as a string, as it is usually written.
func (c Crons) MarshalYAML() (any, error) {
	if len(c) == 1 {
		return c[0], nil
	}
	return []string(c), nil
}

// Set reports whether there is any specification.
func (c Crons) Set() bool { return len(c) > 0 }

// String is the specifications joined by "; ".
func (c Crons) String() string { return strings.Join(c, "; ") }
