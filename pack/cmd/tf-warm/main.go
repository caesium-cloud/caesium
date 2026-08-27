// Command tf-warm is the warm role of the Terraform binding: it populates the
// shared provider filesystem_mirror idempotently and self-checks a marker in
// the cache volume (design §6.3).
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
	protocol.Run("tf-warm", func(*protocol.Emitter) error {
		return errors.New("tf-warm is not implemented yet (infra-deploy Stream C)")
	})
}
