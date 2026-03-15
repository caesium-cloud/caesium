package schema

import "github.com/graphql-go/graphql"

// jobRunParamsType exposes run parameters as a GraphQL scalar (JSON object).
var jobRunParamsType = graphql.NewScalar(graphql.ScalarConfig{
	Name:        "JobRunParams",
	Description: "Key-value pairs passed to a job run as parameters.",
	Serialize: func(value interface{}) interface{} {
		return value
	},
})

// JobRunType exposes a job run including its params.
var JobRunType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "JobRun",
	Description: "A single execution of a job.",
	Fields: graphql.Fields{
		"id": &graphql.Field{
			Type:        graphql.String,
			Description: "The UUID of the job run.",
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if r, ok := p.Source.(map[string]interface{}); ok {
					return r["id"], nil
				}
				return nil, nil
			},
		},
		"params": &graphql.Field{
			Type:        jobRunParamsType,
			Description: "Parameters passed to the run.",
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if r, ok := p.Source.(map[string]interface{}); ok {
					return r["params"], nil
				}
				return nil, nil
			},
		},
	},
})

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
