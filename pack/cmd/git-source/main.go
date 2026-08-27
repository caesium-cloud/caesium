// Command git-source is the materialize role: pin and stage a source tree and
// emit its identity (design §6.1).
//
// Not implemented yet — landing in infra-deploy B2. It fails closed (non-zero
// exit, no marker on stdout) so a manifest wired to this image is red rather
// than silently green: an absent result is never read as "nothing changed"
// (design §5.2).
package main

import (
	"errors"

	"github.com/caesium-cloud/caesium/pack/internal/protocol"
)

func main() {
	protocol.Run("git-source", func(*protocol.Emitter) error {
		return errors.New("git-source is not implemented yet (infra-deploy B2)")
	})
}
