package pki

import (
	"context"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Store wraps catalog access for internal mTLS CA generations and enrollment
// rows.
type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

func (s *Store) CreateCAGenerationIfAbsent(ctx context.Context, gen *models.InternalCAGeneration) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("pki: nil store")
	}
	result := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(gen)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (s *Store) CountCAGenerations(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("pki: nil store")
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.InternalCAGeneration{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) MaxCAGeneration(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("pki: nil store")
	}
	var gen models.InternalCAGeneration
	result := s.db.WithContext(ctx).
		Order("generation DESC").
		Limit(1).
		Find(&gen)
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected == 0 {
		return 0, nil
	}
	return gen.Generation, nil
}

func (s *Store) ActiveCAGenerations(ctx context.Context, now time.Time) ([]models.InternalCAGeneration, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("pki: nil store")
	}
	var gens []models.InternalCAGeneration
	if err := s.db.WithContext(ctx).
		Where("not_after > ?", now.UTC()).
		Order("generation ASC").
		Find(&gens).
		Error; err != nil {
		return nil, err
	}
	return gens, nil
}

func (s *Store) NewestActiveCAGeneration(ctx context.Context, now time.Time) (*models.InternalCAGeneration, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("pki: nil store")
	}
	var gen models.InternalCAGeneration
	result := s.db.WithContext(ctx).
		Where("not_after > ?", now.UTC()).
		Order("generation DESC").
		Limit(1).
		Find(&gen)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &gen, nil
}

func (s *Store) CreateEnrollment(ctx context.Context, enrollment *models.InternalNodeEnrollment) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("pki: nil store")
	}
	return s.db.WithContext(ctx).Create(enrollment).Error
}

func (s *Store) Enrollment(ctx context.Context, id string) (*models.InternalNodeEnrollment, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("pki: nil store")
	}
	var enrollment models.InternalNodeEnrollment
	err := s.db.WithContext(ctx).First(&enrollment, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &enrollment, nil
}

func (s *Store) PendingEnrollments(ctx context.Context, limit int) ([]models.InternalNodeEnrollment, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("pki: nil store")
	}
	if limit <= 0 {
		limit = 32
	}
	var enrollments []models.InternalNodeEnrollment
	if err := s.db.WithContext(ctx).
		Where("status = ?", EnrollmentStatusPending).
		Order("requested_at ASC").
		Limit(limit).
		Find(&enrollments).
		Error; err != nil {
		return nil, err
	}
	return enrollments, nil
}

func (s *Store) MarkEnrollmentSigned(ctx context.Context, id string, caGeneration int, certPEM string, signedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("pki: nil store")
	}
	result := s.db.WithContext(ctx).
		Model(&models.InternalNodeEnrollment{}).
		Where("id = ? AND status = ?", id, EnrollmentStatusPending).
		Updates(map[string]interface{}{
			"ca_generation": caGeneration,
			"cert_pem":      certPEM,
			"status":        EnrollmentStatusSigned,
			"signed_at":     signedAt.UTC(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (s *Store) MarkEnrollmentRejected(ctx context.Context, id string, signedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("pki: nil store")
	}
	result := s.db.WithContext(ctx).
		Model(&models.InternalNodeEnrollment{}).
		Where("id = ? AND status = ?", id, EnrollmentStatusPending).
		Updates(map[string]interface{}{
			"status":    EnrollmentStatusRejected,
			"signed_at": signedAt.UTC(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (s *Store) DeleteEnrollment(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("pki: nil store")
	}
	return s.db.WithContext(ctx).Delete(&models.InternalNodeEnrollment{}, "id = ?", id).Error
}

func (s *Store) PruneExpiredCAGenerations(ctx context.Context, cutoff time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("pki: nil store")
	}
	result := s.db.WithContext(ctx).
		Where("not_after < ?", cutoff.UTC()).
		Delete(&models.InternalCAGeneration{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}
