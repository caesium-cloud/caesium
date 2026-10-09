//go:build integration

package test

func (s *IntegrationTestSuite) TestJobSchemaNamespaceDocumentation() {
	stdout, stderr, err := s.runCLISeparate("job", "schema", "--doc")
	s.Require().NoError(err, "schema documentation failed: %s", stderr)
	s.Require().Contains(stdout, "# Job Definition Schema")
	s.Require().Contains(stdout, "| `namespace` | string | optional |")
	s.Require().Contains(stdout, "Defaults to `default`; DNS label (1-63 lowercase letters, digits or hyphens)")
	s.Require().Contains(stdout, "New runs inherit it; moving a job preserves history and invalidates cached outputs")
}
