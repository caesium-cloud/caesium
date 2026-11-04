package env

import "testing"

func TestSetRunQueueConsumerEnabledRestoresTheCachedFlags(t *testing.T) {
	current := variables
	prevEnabled := current.RunQueueEnabled
	prevDequeuer := current.RunQueueDequeuerEnabled
	t.Cleanup(func() {
		current.RunQueueEnabled = prevEnabled
		current.RunQueueDequeuerEnabled = prevDequeuer
	})

	current.RunQueueEnabled = false
	current.RunQueueDequeuerEnabled = true
	restore := SetRunQueueConsumerEnabled(true)
	if !current.RunQueueEnabled || !current.RunQueueDequeuerEnabled {
		t.Fatalf("consumer flags = %v/%v, want both true", current.RunQueueEnabled, current.RunQueueDequeuerEnabled)
	}
	restore()
	if current.RunQueueEnabled || !current.RunQueueDequeuerEnabled {
		t.Fatalf("restored flags = %v/%v, want false/true", current.RunQueueEnabled, current.RunQueueDequeuerEnabled)
	}
}
