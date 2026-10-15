//go:build race

package http

// raceEnabled: the race detector allocates on its own, so counts mean nothing.
const raceEnabled = true
