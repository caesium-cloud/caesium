package compare

import "fmt"

var (
	ErrNotEqual = fmt.Errorf("objects are not equivalent")
)

func StringSlice(a, b []string) error {
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

func InterfaceSlice(a, b []interface{}) error {
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
