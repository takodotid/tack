//go:build !e2efault

package fault

// Point does nothing in a normal build.
func Point(string, string) {}
