package bodylimit

import (
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func TestReadBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		limit    int64
		want     string
		tooLarge bool
	}{
		{"disabled", "abcde", 0, "abcde", false},
		{"negative", "abcde", -1, "abcde", false},
		{"empty", "", 4, "", false},
		{"below", "abc", 4, "abc", false},
		{"exact", "abcd", 4, "abcd", false},
		{"overflow", "abcde", 4, "abcd", true},
		{"bounded", "abcdefgh", 4, "abcd", true},
		{"max-int", "abcde", math.MaxInt64, "abcde", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := strings.NewReader(tc.body)
			got, err := Read(r, tc.limit)
			if string(got) != tc.want || errors.Is(err, ErrTooLarge) != tc.tooLarge || (!tc.tooLarge && err != nil) {
				t.Fatalf("Read = %q, %v; want %q, overflow=%t", got, err, tc.want, tc.tooLarge)
			}
			if tc.limit > 0 && int64(len(tc.body)-r.Len()) > tc.limit+1 && tc.limit != math.MaxInt64 {
				t.Fatal("consumed beyond overflow probe")
			}
		})
	}
}

type failingReader struct {
	data string
	err  error
}

func (r failingReader) Read(p []byte) (int, error) { return copy(p, r.data), r.err }

func TestReadPreservesIncompleteBodyFailure(t *testing.T) {
	readErr := errors.New("truncated transfer")
	for _, limit := range []int64{0, 4, 10} {
		got, err := Read(failingReader{"abcde", readErr}, limit)
		want := "abcde"
		if limit == 4 {
			want = "abcd"
		}
		if string(got) != want || !errors.Is(err, readErr) || errors.Is(err, ErrTooLarge) {
			t.Fatalf("limit %d: Read = %q, %v", limit, got, err)
		}
	}
	got, err := Read(failingReader{"abc", io.EOF}, 4)
	if string(got) != "abc" || err != nil {
		t.Fatalf("EOF: %q, %v", got, err)
	}
}
