//go:build !race

package engine_test

// raceEnabled reports whether the race detector instruments this build —
// see race_on_test.go for the counterpart.
const raceEnabled = false
