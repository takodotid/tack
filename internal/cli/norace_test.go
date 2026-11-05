//go:build !race

package cli

// raceBuild is set when the tests run with the race detector, which makes
// them about ten times slower.
const raceBuild = false
