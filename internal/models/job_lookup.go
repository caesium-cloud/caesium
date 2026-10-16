package models

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	// ErrAmbiguousJobIDPrefix means a prefix matched more than one job ID.
	ErrAmbiguousJobIDPrefix = errors.New("ambiguous job id prefix")
	// ErrInvalidJobIDPrefix means a job ID prefix cannot match a canonical UUID.
	ErrInvalidJobIDPrefix = errors.New("invalid job id prefix")
)

// FindJobByIDPrefix resolves a job by its full ID or by a unique prefix of its
// canonical (lowercase, hyphenated) ID. It is the one lookup behind every route
// that accepts a job ID prefix, so the auth middleware authorises exactly the
// job the handler serves. Soft-deleted jobs are not found. Errors:
// gorm.ErrRecordNotFound, ErrInvalidJobIDPrefix, ErrAmbiguousJobIDPrefix, or
// the database error.
func FindJobByIDPrefix(db *gorm.DB, rawID string) (*Job, error) {
	rawID = strings.TrimSpace(rawID)
	if rawID == "" {
		return nil, ErrInvalidJobIDPrefix
	}

	if id, err := uuid.Parse(rawID); err == nil {
		job := &Job{ID: id}
		if err := db.First(job).Error; err != nil {
			return nil, err
		}
		return job, nil
	}

	prefix := strings.ToLower(rawID)
	if !isCanonicalUUIDPrefix(prefix) {
		return nil, ErrInvalidJobIDPrefix
	}

	var matches []Job
	if err := db.
		Where("id LIKE ?", prefix+"%").
		Limit(2).
		Find(&matches).Error; err != nil {
		return nil, err
	}

	switch len(matches) {
	case 0:
		return nil, gorm.ErrRecordNotFound
	case 1:
		return &matches[0], nil
	default:
		return nil, ErrAmbiguousJobIDPrefix
	}
}

func isCanonicalUUIDPrefix(prefix string) bool {
	if len(prefix) == 0 || len(prefix) > 36 {
		return false
	}
	for i, r := range prefix {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexRune(r) {
				return false
			}
		}
	}
	return true
}

func isHexRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}
