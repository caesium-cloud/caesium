package models

import "testing"

func TestWebhookEventInAllSlice(t *testing.T) {
	found := false
	for _, m := range All {
		if _, ok := m.(*WebhookEvent); ok {
			found = true
			break
		}
	}
	if !found {
		t.Error("WebhookEvent not found in models.All; AutoMigrate will not create the table")
	}
}
