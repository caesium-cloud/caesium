package auth

import (
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SetKeyWriteHookForTest installs hook between the read and the writes of the
// authorized key operations and returns a func that removes it.
func SetKeyWriteHookForTest(hook func(tx *gorm.DB, id uuid.UUID) error) func() {
	keyWriteHook = hook
	return func() { keyWriteHook = nil }
}
