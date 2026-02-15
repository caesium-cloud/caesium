package worker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseNodeLabels(t *testing.T) {
	labels := ParseNodeLabels("zone=us-west-2, disk=ssd,invalid,nope=,=bad")
	require.Equal(t, map[string]string{
		"zone": "us-west-2",
		"disk": "ssd",
	}, labels)
}
