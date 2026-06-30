package run

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

func TestPriorityValue(t *testing.T) {
	tests := []struct {
		name     string
		priority string
		want     int
	}{
		{name: "empty defaults normal", priority: "", want: PriorityNormalValue},
		{name: "low", priority: jobdef.PriorityLow, want: PriorityLowValue},
		{name: "normal", priority: jobdef.PriorityNormal, want: PriorityNormalValue},
		{name: "high", priority: jobdef.PriorityHigh, want: PriorityHighValue},
		{name: "case and spaces", priority: " HIGH ", want: PriorityHighValue},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PriorityValue(tt.priority)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPriorityValueRejectsInvalidPriority(t *testing.T) {
	_, err := PriorityValue("urgent")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidPriority))
}

func TestPriorityLabel(t *testing.T) {
	require.Equal(t, jobdef.PriorityLow, PriorityLabel(PriorityLowValue))
	require.Equal(t, jobdef.PriorityNormal, PriorityLabel(PriorityNormalValue))
	require.Equal(t, jobdef.PriorityHigh, PriorityLabel(PriorityHighValue))
	require.Equal(t, jobdef.PriorityNormal, PriorityLabel(0))
}
