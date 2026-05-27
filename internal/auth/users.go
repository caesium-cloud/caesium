package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UserStore provisions and updates user identities.
type UserStore struct {
	db  *gorm.DB
	now func() time.Time
}

// NewUserStore creates a user store backed by the given database.
func NewUserStore(db *gorm.DB) *UserStore {
	return &UserStore{db: db, now: time.Now}
}

// Upsert provisions a user on first login and refreshes profile, role, and
// last-login fields on subsequent logins, keyed on (issuer, subject).
func (us *UserStore) Upsert(ctx context.Context, ext *ExternalIdentity, role models.Role) (*models.User, error) {
	now := us.now().UTC()
	groupsJSON, err := json.Marshal(ext.Groups)
	if err != nil {
		return nil, fmt.Errorf("marshal groups: %w", err)
	}

	var user models.User
	err = us.db.WithContext(ctx).Where("issuer = ? AND subject = ?", ext.Issuer, ext.Subject).First(&user).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		user = models.User{
			ID:          uuid.New(),
			Issuer:      ext.Issuer,
			Subject:     ext.Subject,
			Email:       ext.Email,
			DisplayName: ext.DisplayName,
			Groups:      groupsJSON,
			Role:        role,
			CreatedAt:   now,
			LastLoginAt: &now,
		}
		if err := us.db.WithContext(ctx).Create(&user).Error; err != nil {
			if isUniqueConstraintError(err) {
				if existing, lookupErr := us.lookupByIdentity(ctx, ext); lookupErr == nil {
					return us.updateExisting(ctx, &existing, ext, role, groupsJSON, now)
				}
			}
			return nil, fmt.Errorf("create user: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("lookup user: %w", err)
	default:
		return us.updateExisting(ctx, &user, ext, role, groupsJSON, now)
	}

	return &user, nil
}

func (us *UserStore) lookupByIdentity(ctx context.Context, ext *ExternalIdentity) (models.User, error) {
	var user models.User
	err := us.db.WithContext(ctx).Where("issuer = ? AND subject = ?", ext.Issuer, ext.Subject).First(&user).Error
	return user, err
}

func (us *UserStore) updateExisting(ctx context.Context, user *models.User, ext *ExternalIdentity, role models.Role, groupsJSON []byte, now time.Time) (*models.User, error) {
	if user.IsDisabled() {
		return user, nil
	}

	updates := map[string]any{
		"email":         ext.Email,
		"display_name":  ext.DisplayName,
		"groups":        groupsJSON,
		"role":          role,
		"last_login_at": now,
	}
	if err := us.db.WithContext(ctx).Model(user).Updates(updates).Error; err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	user.Email = ext.Email
	user.DisplayName = ext.DisplayName
	user.Groups = groupsJSON
	user.Role = role
	user.LastLoginAt = &now
	return user, nil
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "constraint failed") ||
		strings.Contains(msg, "duplicate")
}
