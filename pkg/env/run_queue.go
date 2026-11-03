package env

// SetRunQueueConsumerEnabled flips the cached run-queue consumer flags.
// It does not reprocess the environment, so a test can turn the consumer on
// or off without applying every other default. restore puts the previous
// values back on the same cached value.
func SetRunQueueConsumerEnabled(enabled bool) (restore func()) {
	current := variables
	prevEnabled := current.RunQueueEnabled
	prevDequeuer := current.RunQueueDequeuerEnabled
	current.RunQueueEnabled = enabled
	current.RunQueueDequeuerEnabled = enabled
	return func() {
		current.RunQueueEnabled = prevEnabled
		current.RunQueueDequeuerEnabled = prevDequeuer
	}
}
