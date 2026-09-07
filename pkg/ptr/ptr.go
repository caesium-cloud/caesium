// Package ptr provides small generic pointer helpers.
package ptr

// Of returns a pointer to a copy of v.
func Of[T any](v T) *T {
	return new(v)
}

// Clone returns a pointer to a copy of *p, or nil if p is nil.
func Clone[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

// Deref returns *p, or the zero value of T if p is nil.
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
