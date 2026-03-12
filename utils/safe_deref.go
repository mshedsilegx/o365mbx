// Package utils provides common utility functions for the application,
// such as safe pointer dereferencing.
package utils

import (
	"time"
)

// StringValue returns the value of the string pointer or a default value if nil.
func StringValue(s *string, defaultValue string) string {
	if s == nil {
		return defaultValue
	}
	return *s
}

// TimeValue returns the value of the time.Time pointer or a default value if nil.
func TimeValue(t *time.Time, defaultValue time.Time) time.Time {
	if t == nil {
		return defaultValue
	}
	return *t
}

// BoolValue returns the value of the bool pointer or a default value if nil.
func BoolValue(b *bool, defaultValue bool) bool {
	if b == nil {
		return defaultValue
	}
	return *b
}

// Int32Value returns the value of the int32 pointer or a default value if nil.
func Int32Value(i *int32, defaultValue int32) int32 {
	if i == nil {
		return defaultValue
	}
	return *i
}
