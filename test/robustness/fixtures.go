package robustness

import (
	"embed"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
)

//go:embed testdata/*.job.yaml
var coreFixtureFS embed.FS

const (
	coreFixturePlaceholderAlias = "__ALIAS__"
	coreFixturePlaceholderImage = "__TASK_IMAGE__"

	fanInStartStep = "start"
	fanInLeftStep  = "left"
	fanInRightStep = "right"
	fanInJoinStep  = "join"
	retryKeepStep  = "keep"
	retryBoomStep  = "boom"
)

// LoadCoreFixture reads a committed testdata job, substitutes alias/image, and
// validates it. The YAML is the fixture the live tests apply; hermetic tests
// parse the same bytes so a drift cannot hide behind a Go-only constructor.
func LoadCoreFixture(name, alias, taskImage string) (jobdef.Definition, error) {
	raw, err := coreFixtureFS.ReadFile("testdata/" + name)
	if err != nil {
		return jobdef.Definition{}, err
	}
	replaced := strings.ReplaceAll(string(raw), coreFixturePlaceholderAlias, alias)
	replaced = strings.ReplaceAll(replaced, coreFixturePlaceholderImage, taskImage)
	def, err := jobdef.Parse([]byte(replaced))
	if err != nil {
		return jobdef.Definition{}, fmt.Errorf("parse testdata/%s: %w", name, err)
	}
	return *def, nil
}
