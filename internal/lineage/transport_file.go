package lineage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

type fileTransport struct {
	mu   sync.Mutex
	file *os.File
}

func NewFileTransport(path string) (Transport, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("lineage: open file: %w", err)
	}
	return &fileTransport{file: f}, nil
}

func (t *fileTransport) Emit(_ context.Context, event RunEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	t.mu.Lock()
	defer t.mu.Unlock()
	_, err = t.file.Write(data)
	return err
}

func (t *fileTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.file.Close()
}
