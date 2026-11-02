package run

import (
	"reflect"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
)

func TestMergeDescriptorSecretRefsEmptyExisting(t *testing.T) {
	first := models.TaskExecutionSecretRef{EnvKey: "K", Ref: "secret://env/K"}
	last := first
	last.Verifiable = true
	other := models.TaskExecutionSecretRef{EnvKey: "OTHER", Ref: first.Ref}
	for _, existing := range [][]models.TaskExecutionSecretRef{nil, {}} {
		updates := []models.TaskExecutionSecretRef{first, other, last}
		got := mergeDescriptorSecretRefs(existing, updates)
		want := []models.TaskExecutionSecretRef{last, other}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		got[0].EnvKey = "changed"
		if !reflect.DeepEqual(updates[0], first) {
			t.Fatal("merge mutated input")
		}
	}
}
