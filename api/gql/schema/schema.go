package schema

import "github.com/graphql-go/graphql"

// New instantiates a fresh GraphQL schema for
// Caesium's API.
func New() graphql.SchemaConfig {
	return graphql.SchemaConfig{
		Query: graphql.NewObject(
			graphql.ObjectConfig{
				Name:   "Query",
				Fields: fields(),
			},
		),
	}
}

func fields() graphql.Fields {
	return graphql.Fields{
		"place": &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return "holder", nil
			},
		},
	}
}
