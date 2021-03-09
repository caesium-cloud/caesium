package gql

import (
	"github.com/caesium-dev/caesium/api/gql/schema"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/handler"
	"github.com/labstack/echo/v4"
)

// Handler wraps the GraphQL schema and makes it injectable
// into the echo HTTP framework.
func Handler() echo.HandlerFunc {
	schema, err := graphql.NewSchema(schema.New())
	if err != nil {
		panic(err)
	}

	return echo.WrapHandler(
		handler.New(
			&handler.Config{
				Schema:   &schema,
				Pretty:   true,
				GraphiQL: true,
			},
		),
	)
}
