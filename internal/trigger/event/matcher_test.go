package event

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"gorm.io/datatypes"
)

func TestEventPatternMatchesExactAndGlobType(t *testing.T) {
	t.Parallel()

	evt := &models.IngestedEvent{Type: "webhook.github", Data: datatypes.JSON(`{}`)}
	if !(EventPattern{Type: "webhook.github"}).Matches(evt) {
		t.Fatal("exact event type should match")
	}
	if !(EventPattern{Type: "webhook.*"}).Matches(evt) {
		t.Fatal("glob event type should match")
	}
	if (EventPattern{Type: "lifecycle.*"}).Matches(evt) {
		t.Fatal("unmatched glob should not match")
	}
}

func TestEventPatternSourceMismatch(t *testing.T) {
	t.Parallel()

	evt := &models.IngestedEvent{Type: "webhook.github", Source: "github", Data: datatypes.JSON(`{}`)}
	if (EventPattern{Type: "webhook.*", Source: "gitlab"}).Matches(evt) {
		t.Fatal("source mismatch should not match")
	}
}

func TestEventPatternNestedFilter(t *testing.T) {
	t.Parallel()

	evt := &models.IngestedEvent{
		Type: "webhook.github",
		Data: datatypes.JSON(`{"repository":{"full_name":"caesium-cloud/caesium"},"action":"opened"}`),
	}
	pattern := EventPattern{
		Type: "webhook.*",
		Filter: map[string]string{
			"repository.full_name": "caesium-cloud/caesium",
			"action":               "opened",
		},
	}
	if !pattern.Matches(evt) {
		t.Fatal("nested filter should match")
	}
}

func TestEventPatternMissingFilterField(t *testing.T) {
	t.Parallel()

	evt := &models.IngestedEvent{Type: "webhook.github", Data: datatypes.JSON(`{"repository":{}}`)}
	pattern := EventPattern{
		Type:   "webhook.*",
		Filter: map[string]string{"repository.full_name": "caesium-cloud/caesium"},
	}
	if pattern.Matches(evt) {
		t.Fatal("missing filter field should not match")
	}
}

func TestEventPatternFilterCoercion(t *testing.T) {
	t.Parallel()

	evt := &models.IngestedEvent{
		Type: "webhook.github",
		Data: datatypes.JSON(`{"delivery":{"attempt":2,"redelivered":false}}`),
	}
	pattern := EventPattern{
		Type: "webhook.*",
		Filter: map[string]string{
			"delivery.attempt":     "2",
			"delivery.redelivered": "false",
		},
	}
	if !pattern.Matches(evt) {
		t.Fatal("numeric and boolean fields should coerce to strings")
	}
}

func TestExtractFieldRejectsMissingNestedPath(t *testing.T) {
	t.Parallel()

	if _, ok := extractField([]byte(`{"repository":{}}`), "repository.full_name"); ok {
		t.Fatal("missing nested path should not extract")
	}
}
