package main

import "testing"

// The examples are documentation that runs: a change to the format that
// breaks one fails here rather than in the hands of whoever copies it.
func TestExamplesAreValid(t *testing.T) {
	err := cmdValidate([]string{
		"-config", "../../examples/argus.yaml",
		"-probes", "../../examples/probes.d",
		"-log-level", "error",
	})
	if err != nil {
		t.Fatal(err)
	}
}
