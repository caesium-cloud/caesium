// Command tf-runner is the propose/apply role of the Terraform binding:
// `tf-plan` produces a reviewable plan artifact, `tf-apply` consumes exactly
// that artifact, `tf-drift` refresh-plans for out-of-band change (design §6.4,
// §6.6).
//
// Not implemented yet — landing in infra-deploy Stream C. It fails closed
// (non-zero exit, no marker on stdout) so a manifest wired to this image is red
// rather than silently green: an absent result is never read as "nothing
// changed" (design §5.2).
package main

import (
	"errors"

	"github.com/caesium-cloud/caesium/pack/internal/protocol"
)

func main() {
	protocol.Run("tf-runner", func(*protocol.Emitter) error {
		return errors.New("tf-runner is not implemented yet (infra-deploy Stream C)")
	})
}
