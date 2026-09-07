package jsonmap

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestFromStringMap(t *testing.T) {
	require.Equal(t, datatypes.JSONMap{}, FromStringMap(nil))
	require.Equal(t, datatypes.JSONMap{"zone": "us-east-1"}, FromStringMap(map[string]string{"zone": "us-east-1"}))
}

func TestFromMap(t *testing.T) {
	require.Equal(t, datatypes.JSONMap{}, FromMap[int](nil))
	require.Equal(t, datatypes.JSONMap{"attempt": 2}, FromMap(map[string]int{"attempt": 2}))
}

func TestLookup(t *testing.T) {
	m := datatypes.JSONMap{"zone": "us-east-1", "attempt": 2}

	zone, ok := Lookup[string](m, "zone")
	require.True(t, ok)
	require.Equal(t, "us-east-1", zone)

	_, ok = Lookup[int](m, "zone")
	require.False(t, ok)

	_, ok = Lookup[string](m, "missing")
	require.False(t, ok)
}

func TestToStringMap(t *testing.T) {
	require.Equal(t, map[string]string{}, ToStringMap(nil))
	require.Equal(t, map[string]string{"attempt": "2", "zone": "us-east-1"}, ToStringMap(datatypes.JSONMap{
		"zone":    "us-east-1",
		"attempt": 2,
	}))
}
