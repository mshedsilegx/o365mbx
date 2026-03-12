// Package main is the entry point for the o365mbx application.
// It handles CLI argument parsing, configuration loading, and dependency injection.
//
// This file contains the non-Windows implementation of pre-flight checks.
//go:build !windows

package main

// checkLongPathSupport is a no-op on non-Windows systems, as they typically
// support long paths by default without special configuration.
func checkLongPathSupport() {
	// This function is intentionally empty.
}
