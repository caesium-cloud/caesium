package tf

import (
	"io"
	"sync"

	"github.com/hashicorp/terraform-exec/tfexec"
)

// NewTerraform routes both Terraform child streams to one serialized log writer.
// Protocol stdout remains exclusively owned by the reagent emitter.
func NewTerraform(dir, execPath string, log io.Writer) (*tfexec.Terraform, io.Writer, error) {
	terraform, err := tfexec.NewTerraform(dir, execPath)
	if err != nil {
		return nil, nil, err
	}
	serialized := &syncWriter{w: log}
	terraform.SetStdout(serialized)
	terraform.SetStderr(serialized)
	return terraform, serialized, nil
}

// syncWriter serializes concurrent writes from terraform-exec's two output
// pumps. A single *os.File would be safe on its own (one write syscall), but the
// Runner's contract must not depend on which writer a caller happens to pass.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
