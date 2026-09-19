//go:build !unix

package external

import "os/exec"

// ownGroup is a no-op where process groups do not exist.
func ownGroup(*exec.Cmd) {}
