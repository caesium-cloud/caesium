package models

// IsFannedGroup reports whether a nonempty group represents partitioned work.
func IsFannedGroup(count int, partitionValue string) bool {
	return count > 1 || (count == 1 && partitionValue != "")
}
