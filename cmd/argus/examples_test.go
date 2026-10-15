package main

import (
	"os"
	"strings"
	"testing"
)

func validateExamples() error {
	return cmdValidate([]string{
		"-config", "../../examples/argus.yaml",
		"-probes", "../../examples/probes.d",
		"-log-level", "error",
	})
}

// The examples are documentation that runs: a change to the format that
// breaks one fails here rather than in the hands of whoever copies it.
func TestExamplesAreValid(t *testing.T) {
	t.Setenv("ARGUS_API_TOKEN", "example-token")
	if err := validateExamples(); err != nil {
		t.Fatal(err)
	}
}

// Copied without its environment, the example must not run with the literal
// "${ARGUS_API_TOKEN}" — a token printed in the repository — guarding the API.
func TestExampleRefusesAnUnsetToken(t *testing.T) {
	t.Setenv("ARGUS_API_TOKEN", "")
	if err := validateExamples(); err == nil || !strings.Contains(err.Error(), "http.api.token") {
		t.Errorf("empty: err = %v, want the token refused", err)
	}
	os.Unsetenv("ARGUS_API_TOKEN") // t.Setenv restores it afterwards
	if err := validateExamples(); err == nil || !strings.Contains(err.Error(), "${ARGUS_API_TOKEN} is not set") {
		t.Errorf("unset: err = %v, want the unset variable named", err)
	}
}
