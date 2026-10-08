package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestPromoteMirrorRetainsPromotionAndRollbackCauses(t *testing.T) {
	cache := t.TempDir()
	mirror := filepath.Join(cache, "mirror")
	staging := filepath.Join(cache, "staging")
	for _, path := range []string{mirror, staging} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	primary := errors.New("promotion failed")
	secondary := errors.New("rollback failed")
	calls := 0
	quarantine := ""
	rename := func(from, to string) error {
		calls++
		switch calls {
		case 1:
			return fs.ErrExist // Existing incomplete mirror enters repair.
		case 2:
			quarantine = to
			return os.Rename(from, to)
		case 3:
			return primary
		case 4:
			return secondary
		default:
			t.Fatalf("unexpected rename %s -> %s", from, to)
			return nil
		}
	}
	err := promoteMirrorWithRename(staging, mirror, cache, "key", nil, nil, io.Discard, rename)
	if !errors.Is(err, primary) || !errors.Is(err, secondary) {
		t.Fatalf("lost cause: %v", err)
	}
	want := fmt.Sprintf("promote repaired mirror %s: promotion failed (also could not restore quarantine %s: rollback failed)", mirror, quarantine)
	if err.Error() != want {
		t.Fatalf("diagnostic = %q, want %q", err, want)
	}
	if calls != 4 {
		t.Fatalf("rename calls = %d", calls)
	}
}
