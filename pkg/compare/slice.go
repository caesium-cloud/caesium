package compare

import "fmt"

var (
	ErrNotEqual = fmt.Errorf("objects are not equivalent")
)

func Slice[T comparable](a, b []T) error {
	if len(a) != len(b) {
		return ErrNotEqual
	}
	for i, v := range a {
		if v != b[i] {
			return ErrNotEqual
		}
	}
	return nil
}
