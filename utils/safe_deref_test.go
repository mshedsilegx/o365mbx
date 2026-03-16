package utils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStringValue(t *testing.T) {
	s := "test"
	assert.Equal(t, "test", StringValue(&s, "default"))
	assert.Equal(t, "default", StringValue(nil, "default"))
}

func TestTimeValue(t *testing.T) {
	now := time.Now().UTC()
	assert.Equal(t, now, TimeValue(&now, time.Time{}))
	assert.Equal(t, time.Time{}, TimeValue(nil, time.Time{}))
}

func TestBoolValue(t *testing.T) {
	b := true
	assert.Equal(t, true, BoolValue(&b, false))
	assert.Equal(t, false, BoolValue(nil, false))
}

func TestInt32Value(t *testing.T) {
	i := int32(42)
	assert.Equal(t, int32(42), Int32Value(&i, 0))
	assert.Equal(t, int32(0), Int32Value(nil, 0))
}
