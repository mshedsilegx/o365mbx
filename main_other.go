// Package main is the entry point for the o365mbx application.
//
// This file contains the non-Windows implementation of pre-flight checks.
// On non-Windows systems, long path support is typically handled by the OS
// without additional configuration.
//go:build !windows

package main

// checkLongPathSupport is a no-op on non-Windows systems, as they typically
// support long paths by default without special configuration.
var checkLongPathSupport = func() {
	// This function is intentionally empty.
}
