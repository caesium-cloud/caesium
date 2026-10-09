package policy

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestYAMLValidatorRejectsUnsupportedFieldTypes(t *testing.T) {
	t.Parallel()
	for _, typ := range []reflect.Type{
		reflect.TypeFor[bool](), reflect.TypeFor[int64](), reflect.TypeFor[uint](),
		reflect.TypeFor[float64](), reflect.TypeFor[any](),
	} {
		t.Run(typ.String(), func(t *testing.T) {
			t.Parallel()
			walker := yamlValidator{active: make(map[*yaml.Node]bool), validated: make(map[yamlValidationKey]bool)}
			node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
			require.ErrorContains(t, walker.validate(node, typ, "futureField"), "unsupported policy field type")
		})
	}
}
