package dataset

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateTolerancesRejectsNonsense pins the `--tolerate` grammar at the
// endpoint.
//
// The windows are RECORDED, not yet enforced — nothing reads DatasetHold
// .Tolerances back in v1 — which is exactly why they have to be validated on the
// way in. A field that is stored but not consulted will otherwise accumulate
// unparseable values until the release that finally reads them, and the operator
// who typed "1 day" would learn months later that their ack recorded nothing
// meaningful.
func TestValidateTolerancesRejectsNonsense(t *testing.T) {
	t.Run("empty is allowed", func(t *testing.T) {
		got, err := validateTolerances(nil)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("known assertion with a positive duration", func(t *testing.T) {
		got, err := validateTolerances(map[string]string{
			"rowCount":          "24h",
			"deltaFromBaseline": "30m",
		})
		require.Error(t, err, "rowCount is a METRIC, not an assertion kind")
		require.Nil(t, got)

		got, err = validateTolerances(map[string]string{
			"min":               "24h",
			"deltaFromBaseline": "30m",
			"maxLag":            "1h30m",
		})
		require.NoError(t, err)
		require.Equal(t, map[string]string{
			"min":               "24h",
			"deltaFromBaseline": "30m",
			"maxLag":            "1h30m",
		}, got)
	})

	t.Run("unknown assertion kind", func(t *testing.T) {
		_, err := validateTolerances(map[string]string{"whenever": "24h"})
		require.ErrorContains(t, err, "is not an assertion kind")
	})

	t.Run("unparseable duration", func(t *testing.T) {
		_, err := validateTolerances(map[string]string{"min": "1 day"})
		require.ErrorContains(t, err, "is not a duration")
	})

	t.Run("non-positive duration", func(t *testing.T) {
		_, err := validateTolerances(map[string]string{"min": "0s"})
		require.ErrorContains(t, err, "must be a positive duration")

		_, err = validateTolerances(map[string]string{"max": "-1h"})
		require.ErrorContains(t, err, "must be a positive duration")
	})
}
