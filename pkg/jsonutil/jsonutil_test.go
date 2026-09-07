package jsonutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnmarshalRoundTrip(t *testing.T) {
	type sample struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}

	encoded, err := MarshalString(sample{Name: "caesium", N: 2})
	require.NoError(t, err)

	got, err := UnmarshalString[sample](encoded)
	require.NoError(t, err)
	require.Equal(t, sample{Name: "caesium", N: 2}, got)
}

func TestUnmarshalRejectsInvalidJSON(t *testing.T) {
	_, err := Unmarshal[map[string]int]([]byte(`{`))
	require.Error(t, err)
}
