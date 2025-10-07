package lint

import (
	"context"
	"errors"
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/suite"
)

type SecretSuite struct {
	suite.Suite
}

func TestSecretSuite(t *testing.T) {
	suite.Run(t, new(SecretSuite))
}

func (s *SecretSuite) TestCollectSecretReferences() {
	def := schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: "test"},
		Trigger:    schema.Trigger{Configuration: map[string]any{"auth": "secret://env/TOKEN"}},
		Callbacks: []schema.Callback{{
			Type:          schema.CallbackNotification,
			Configuration: map[string]any{"webhook": "secret://vault/secret/data/hook?field=url"},
		}},
	}

	references, err := CollectSecretReferences(def)
	s.Require().NoError(err)
	s.ElementsMatch([]string{"secret://env/TOKEN", "secret://vault/secret/data/hook?field=url"}, references)
}

func (s *SecretSuite) TestCheckSecretsAggregatesErrors() {
	def := schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: "pipeline"},
	}
	r := &fakeResolver{responses: map[string]error{
		"secret://env/TOKEN":                        nil,
		"secret://vault/secret/data/hook?field=url": errors.New("not found"),
	}}

	def.Trigger.Configuration = map[string]any{"token": "secret://env/TOKEN"}
	def.Callbacks = []schema.Callback{{
		Type:          schema.CallbackNotification,
		Configuration: map[string]any{"webhook": "secret://vault/secret/data/hook?field=url"},
	}}

	errs := CheckSecrets(context.Background(), r, []schema.Definition{def})
	s.Require().Len(errs, 1)
	s.Contains(errs[0], "secret://vault/secret/data/hook?field=url")
}

type fakeResolver struct {
	responses map[string]error
}

func (f *fakeResolver) Resolve(_ context.Context, ref string) (string, error) {
	if err, ok := f.responses[ref]; ok {
		return "", err
	}
	return "", nil
}
