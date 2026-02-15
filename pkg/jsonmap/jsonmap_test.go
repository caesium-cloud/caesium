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

func TestToStringMap(t *testing.T) {
	require.Equal(t, map[string]string{}, ToStringMap(nil))
	require.Equal(t, map[string]string{"attempt": "2", "zone": "us-east-1"}, ToStringMap(datatypes.JSONMap{
		"zone":    "us-east-1",
		"attempt": 2,
	}))
}
