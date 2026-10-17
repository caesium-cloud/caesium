package middleware

import (
	"encoding/json"
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// TestJobdefReadBodyBindingParity pins that the resolver's decoding shape
// (jobdefReadBody) sees the same alias and namespace as the lint and diff
// handlers, which c.Bind (json.Unmarshal) into []pkg/jobdef.Definition — the
// middleware cannot import controller/jobdef's request types. Whenever the
// handler can decode a body, the resolver can too, with equal values.
func TestJobdefReadBodyBindingParity(t *testing.T) {
	bodies := []string{
		`{"definitions":[{"metadata":{"alias":"a","namespace":"marketing"}}]}`,
		`{"definitions":[{"metadata":{"alias":"a"}}]}`,
		`{"definitions":[{"metadata":{"alias":"a","namespace":null}}]}`,
		`{"definitions":[{"Metadata":{"ALIAS":"a","Namespace":"finance"}}]}`,
		`{"DEFINITIONS":[{"metadata":{"alias":"a","namespace":"marketing","namespace":"finance"}}]}`,
		`{"definitions":[{"metadata":{"alias":"a","namespace":"marketing"},"metadata":{"alias":"b","namespace":"finance"}}]}`,
		"{\"definitions\":[{\"metadata\":{\"alias\":\"\\u0061\",\"namespace\":\"fin\\u0061nce\"}}]}",
		`{"definitions":[{"metadata":{"alias":" a ","namespace":" marketing","taskTimeout":"5m","labels":{"x":"y"}}}]}`,
		`{"definitions":[{"metadata":{"alias":"a","namespace":"marketing"}},{"metadata":{"alias":"b","namespace":""}}],"extra":1}`,
		`{"definitions":[{"metadata":{"alias":"a","namespace":"marketing"},"metadata":{"alias":"b"}}]}`,
		`{"definitions":[{"metadata":{"alias":"a","namespace":"marketing"},"metadata":null}]}`,
		`{"definitions":[{"metadata":null}]}`,
		`{"definitions":[{"metadata":{"alias":"a","namespace":"marketing"}}],"definitions":[{"metadata":{"alias":"b"}}]}`,
		`{"definitions":null}`,
		`{}`,
	}
	for _, body := range bodies {
		var handler struct {
			Definitions []schema.Definition `json:"definitions"`
		}
		handlerErr := json.Unmarshal([]byte(body), &handler)
		var resolver jobdefReadBody
		resolverErr := json.Unmarshal([]byte(body), &resolver)
		if handlerErr != nil {
			continue
		}
		require.NoError(t, resolverErr, body)
		require.Len(t, resolver.Definitions, len(handler.Definitions), body)
		for i := range handler.Definitions {
			require.Equal(t, handler.Definitions[i].Metadata.Alias, resolver.Definitions[i].Metadata.Alias, body)
			require.Equal(t, handler.Definitions[i].Metadata.Namespace, resolver.Definitions[i].Metadata.Namespace, body)
		}
	}
}
