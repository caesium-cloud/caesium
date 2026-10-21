package models

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Shared-object names (notification channels and policies, agent profiles)
// are globally unique: jobs reference channels and profiles by name, and the
// name column carries a plain unique index (identity-and-access D4). A
// soft-deleted row still occupies that index, so a delete renames its
// tombstone out of the way (TombstoneName) and a name claim frees the name of
// a tombstone left before tombstones were renamed. Deleting a squatted name
// therefore makes it available again.

// tombstoneMarker separates a tombstone's former name from its object id.
const tombstoneMarker = "~deleted~"

// TombstoneName is the name a soft-deleted shared object is renamed to, so it
// no longer holds the live name. The object id keeps it unique, because no
// live name may carry the marker (CheckSharedObjectName).
func TombstoneName(name string, id uuid.UUID) string {
	return TombstonePrefix(name) + id.String()
}

// TombstonePrefix is the prefix every TombstoneName of name starts with.
func TombstonePrefix(name string) string {
	return name + tombstoneMarker
}

// CheckSharedObjectName rejects a name no live shared object may take: one
// carrying the tombstone marker. Otherwise a caller could claim the name a
// later delete renames another object's tombstone to, and make that delete
// fail.
func CheckSharedObjectName(name string) error {
	if strings.Contains(name, tombstoneMarker) {
		return fmt.Errorf("name must not contain %q, which is reserved for deleted objects", tombstoneMarker)
	}
	return nil
}

// ClaimSharedObjectName makes name available to the row excludeID
// (uuid.Nil on create) of model's table, inside tx. It reports taken when a
// live row other than excludeID holds the name; a soft-deleted row holding it
// is renamed to its TombstoneName instead.
func ClaimSharedObjectName(tx *gorm.DB, model any, name string, excludeID uuid.UUID) (taken bool, err error) {
	var holders []struct {
		ID        uuid.UUID
		DeletedAt gorm.DeletedAt
	}
	query := tx.Unscoped().Model(model).Select("id", "deleted_at").Where("name = ?", name)
	if excludeID != uuid.Nil {
		query = query.Where("id <> ?", excludeID)
	}
	if err := query.Find(&holders).Error; err != nil {
		return false, err
	}
	for _, holder := range holders {
		if !holder.DeletedAt.Valid {
			return true, nil
		}
	}
	for _, holder := range holders {
		if err := tx.Unscoped().Model(model).Where("id = ?", holder.ID).
			UpdateColumn("name", TombstoneName(name, holder.ID)).Error; err != nil {
			return false, err
		}
	}
	return false, nil
}

// SoftDeleteSharedObject soft-deletes the live row id of model's table and
// renames it to its TombstoneName, releasing its name. Run it inside tx (the
// caller owns the transaction and its contention retry, pkg/db TransactionOn).
// A missing (or already deleted) row is not an error, as gorm's Delete.
func SoftDeleteSharedObject(tx *gorm.DB, model any, id uuid.UUID) error {
	var names []string
	if err := tx.Model(model).Where("id = ?", id).Limit(1).Pluck("name", &names).Error; err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	return tx.Unscoped().Model(model).Where("id = ? AND deleted_at IS NULL", id).
		UpdateColumns(map[string]any{
			"name":       TombstoneName(names[0], id),
			"deleted_at": time.Now().UTC(),
		}).Error
}
