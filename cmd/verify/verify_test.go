package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ireceipt "github.com/caesium-cloud/caesium/internal/receipt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestLoadReceiptRoundTrip: a written receipt file loads back with its IDs and
// digest intact.
func TestLoadReceiptRoundTrip(t *testing.T) {
	r := &ireceipt.Receipt{
		ReceiptVersion: ireceipt.Version,
		RunID:          uuid.New(),
		JobID:          uuid.New(),
		ReceiptDigest:  "abc123",
	}
	data, err := json.MarshalIndent(r, "", "  ")
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	loaded, err := loadReceipt(path)
	require.NoError(t, err)
	require.Equal(t, r.RunID, loaded.RunID)
	require.Equal(t, r.JobID, loaded.JobID)
	require.Equal(t, "abc123", loaded.ReceiptDigest)
}

// TestLoadReceiptMissingFile: a missing file is a clear error, not a panic.
func TestLoadReceiptMissingFile(t *testing.T) {
	_, err := loadReceipt(filepath.Join(t.TempDir(), "does-not-exist.json"))
	require.Error(t, err)
}

// TestLoadReceiptBadJSON: malformed JSON is reported as a parse error.
func TestLoadReceiptBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	_, err := loadReceipt(path)
	require.Error(t, err)
}

func TestShortDigest(t *testing.T) {
	require.Equal(t, "short", shortDigest("short"))
	require.Equal(t, "0123456789abcdef…", shortDigest("0123456789abcdef0123456789abcdef"))
}
