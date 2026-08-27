// Command tf-discover is the discover role: it enumerates Terraform stacks and
// fingerprints each one (design §6.2).
//
// Not implemented yet — landing in infra-deploy B4. It fails closed (non-zero
// exit, no marker on stdout) so a manifest wired to this image is red rather
// than silently green: an absent fingerprint is never read as "nothing changed"
// (design §5.2, §8).
package main

import (
	"errors"

	"github.com/caesium-cloud/caesium/pack/internal/protocol"
)

func main() {
	protocol.Run("tf-discover", func(*protocol.Emitter) error {
		return errors.New("tf-discover is not implemented yet (infra-deploy B4)")
	})
}
