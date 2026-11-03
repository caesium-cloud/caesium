package run

import (
	"errors"

	"github.com/caesium-cloud/caesium/pkg/env"
)

// RunQueueConsumerDisabledMessage is the refusal an operator sees when a job's
// queue strategy would park a run and neither queue flag starts the consumer
// that would drain it. A quota-only dequeuer does not count: it never promotes
// a row the job's own policy queued.
const RunQueueConsumerDisabledMessage = "run queue consumer is disabled (CAESIUM_RUN_QUEUE_ENABLED and CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED are both false); this run will not start"

// ErrRunQueueConsumerDisabled refuses a new job-policy queue admission.
// Promotions of rows already queued (fromQueue) do not return it.
var ErrRunQueueConsumerDisabled = errors.New(RunQueueConsumerDisabledMessage)

// RunQueueConsumerEnabled reports whether a full dequeuer will drain rows a
// job's own queue strategy parked. Either flag starts that consumer. Both
// default to false. A quota-only dequeuer (access-policy file, flags off)
// does not.
func RunQueueConsumerEnabled() bool {
	vars := env.Variables()
	return vars.RunQueueEnabled || vars.RunQueueDequeuerEnabled
}
