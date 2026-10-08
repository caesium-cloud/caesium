package orderby

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllowlistedOrderTerms(t *testing.T) {
	notificationColumns := map[string]struct{}{
		"name": {}, "type": {}, "enabled": {}, "created_at": {}, "updated_at": {},
	}
	agentProfileColumns := map[string]struct{}{
		"name": {}, "image": {}, "engine": {}, "created_at": {}, "updated_at": {},
	}

	tests := []struct {
		name    string
		raw     string
		allowed map[string]struct{}
		want    []string
		wantErr string
	}{
		{name: "empty is allocated", raw: " , , ", allowed: notificationColumns, want: []string{}},
		{name: "notification columns normalize", raw: " TYPE desc, enabled ", allowed: notificationColumns, want: []string{"type desc", "enabled asc"}},
		{name: "agent profile columns normalize", raw: " IMAGE DESC, Engine ", allowed: agentProfileColumns, want: []string{"image desc", "engine asc"}},
		{name: "notification rejects image", raw: "image", allowed: notificationColumns, wantErr: `invalid order_by column: "image"`},
		{name: "agent profile rejects type", raw: "type", allowed: agentProfileColumns, wantErr: `invalid order_by column: "type"`},
		{name: "bad direction", raw: "name sideways", allowed: agentProfileColumns, wantErr: `invalid order_by direction: "sideways"`},
		{name: "extra token", raw: "name desc nulls", allowed: agentProfileColumns, wantErr: `invalid order_by term: "name desc nulls"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.raw, tt.allowed)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			if len(tt.want) == 0 {
				assert.NotNil(t, got)
			}
		})
	}
}
