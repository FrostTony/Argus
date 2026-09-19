package app

import (
	"context"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

func TestAdhocCheckIsNotHeldToTheNodeInterval(t *testing.T) {
	node := testServer()
	node.Probing.Interval = config.Duration(30 * time.Second)
	node.Probing.Timeout = config.Duration(5 * time.Second)
	a, err := Build(node, probesFrom(t, `probes: [{name: a, type: counting, targets: ["1.1.1.1"]}]`), discard())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	spec, err := config.ParseProbe([]byte("type: counting\ntimeout: 45s\ntargets: [\"1.1.1.1\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Check(context.Background(), CheckRequest{Spec: &spec}); err != nil {
		t.Fatalf("a one-off check with a 45s timeout: %v", err)
	}
}
