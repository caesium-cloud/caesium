package testutil

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMustJSONBytes(t *testing.T) {
	require.Equal(t, []byte("null"), MustJSONBytes(t, nil))
	require.Equal(t, []byte(`{"key":"value"}`), MustJSONBytes(t, map[string]string{"key": "value"}))
}
