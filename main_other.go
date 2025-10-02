//go:build !windows

package main

// checkLongPathSupport is a no-op on non-Windows systems, as they typically
// support long paths by default without special configuration.
func checkLongPathSupport() {
	// This function is intentionally empty.
}