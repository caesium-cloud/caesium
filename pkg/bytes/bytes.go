package bytes

import "encoding/binary"

// ToUint64 converts bytes to an integer
func ToUint64(b []byte) uint64 {
	return binary.BigEndian.Uint64(b)
}

// FromUint64 converts a uint to a byte slice
func FromUint64(u uint64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, u)
	return buf
}
