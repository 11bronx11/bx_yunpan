package identity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrConflict     = errors.New("identity conflict")
	ErrUnauthorized = errors.New("invalid credentials")
	ErrDisabled     = errors.New("user disabled")
	ErrInvalidInput = errors.New("invalid identity input")
)

type Service struct {
	db                *gorm.DB
	tokens            *TokenManager
	provisioner       UserProvisioner
	defaultQuotaBytes int64
}

type UserProvisioner interface {
	ProvisionUser(*gorm.DB, uuid.UUID) error
}

type Session struct {
	User          User
	AccessToken   string
	AccessExpires time.Time
	RefreshToken  string
	RefreshExpiry time.Time
}

func NewService(db *gorm.DB, tokens *TokenManager, provisioner UserProvisioner, defaultQuotaBytes int64) *Service {
	return &Service{db: db, tokens: tokens, provisioner: provisioner, defaultQuotaBytes: defaultQuotaBytes}
}

func (s *Service) Register(username, email, password string) (Session, error) {
	username = strings.TrimSpace(username)
	usernameNormalized := strings.ToLower(username)
	if len(username) < 3 || len(username) > 64 || strings.ContainsAny(username, "/\\\x00") {
		return Session{}, ErrInvalidInput
	}
	if len(password) < 10 || len(password) > 1024 {
		return Session{}, errWeakPassword
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return Session{}, fmt.Errorf("hash password: %w", err)
	}
	var emailNormalized *string
	if value := strings.ToLower(strings.TrimSpace(email)); value != "" {
		if len(value) > 254 || !strings.Contains(value, "@") {
			return Session{}, ErrInvalidInput
		}
		emailNormalized = &value
	}

	user := User{
		ID:                 uuid.Must(uuid.NewV7()),
		Username:           username,
		UsernameNormalized: usernameNormalized,
		EmailNormalized:    emailNormalized,
		PasswordHash:       passwordHash,
		Status:             UserStatusActive,
		QuotaBytes:         s.defaultQuotaBytes,
	}
	var session Session
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrConflict
			}
			return err
		}
		if s.provisioner != nil {
			if err := s.provisioner.ProvisionUser(tx, user.ID); err != nil {
				return err
			}
		}
		created, err := s.issueSession(tx, user, uuid.Must(uuid.NewV7()))
		if err != nil {
			return err
		}
		session = created
		return nil
	})
	return session, err
}

func (s *Service) Login(login, password string) (Session, error) {
	if len(login) == 0 || len(login) > 254 || len(password) == 0 || len(password) > 1024 {
		return Session{}, ErrUnauthorized
	}
	normalized := strings.ToLower(strings.TrimSpace(login))
	var user User
	if err := s.db.Where("username_normalized = ? OR email_normalized = ?", normalized, normalized).First(&user).Error; err != nil {
		return Session{}, ErrUnauthorized
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return Session{}, ErrUnauthorized
	}
	if user.Status != UserStatusActive {
		return Session{}, ErrDisabled
	}
	return s.issueSession(s.db, user, uuid.Must(uuid.NewV7()))
}

func (s *Service) Rotate(rawToken string) (Session, error) {
	if rawToken == "" {
		return Session{}, ErrUnauthorized
	}
	var result Session
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var current RefreshSession
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", RefreshHash(rawToken)).First(&current).Error
		if err != nil {
			return ErrUnauthorized
		}
		now := time.Now().UTC()
		if current.RevokedAt != nil {
			tx.Model(&RefreshSession{}).Where("family_id = ? AND revoked_at IS NULL", current.FamilyID).Update("revoked_at", now)
			return ErrUnauthorized
		}
		if !current.ExpiresAt.After(now) {
			return ErrUnauthorized
		}
		if err := tx.Model(&current).Update("revoked_at", now).Error; err != nil {
			return err
		}
		var user User
		if err := tx.First(&user, "id = ?", current.UserID).Error; err != nil || user.Status != UserStatusActive {
			return ErrUnauthorized
		}
		result, err = s.issueSession(tx, user, current.FamilyID)
		return err
	})
	return result, err
}

func (s *Service) Logout(rawToken string) error {
	if rawToken == "" {
		return nil
	}
	now := time.Now().UTC()
	return s.db.Model(&RefreshSession{}).
		Where("token_hash = ? AND revoked_at IS NULL", RefreshHash(rawToken)).
		Update("revoked_at", now).Error
}

func (s *Service) User(userID uuid.UUID) (User, error) {
	var user User
	if err := s.db.First(&user, "id = ? AND status = ?", userID, UserStatusActive).Error; err != nil {
		return User{}, ErrUnauthorized
	}
	return user, nil
}

func (s *Service) ReserveQuota(tx *gorm.DB, userID uuid.UUID, size int64) error {
	result := tx.Model(&User{}).
		Where("id = ? AND status = ? AND used_logical_bytes + reserved_bytes + ? <= quota_bytes", userID, UserStatusActive, size).
		Update("reserved_bytes", gorm.Expr("reserved_bytes + ?", size))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Service) ReleaseQuota(tx *gorm.DB, userID uuid.UUID, reserved int64) error {
	return tx.Model(&User{}).Where("id = ?", userID).
		Update("reserved_bytes", gorm.Expr("GREATEST(reserved_bytes - ?, 0)", reserved)).Error
}

func (s *Service) CommitQuota(tx *gorm.DB, userID uuid.UUID, reserved, logical int64) error {
	return tx.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"reserved_bytes":     gorm.Expr("GREATEST(reserved_bytes - ?, 0)", reserved),
		"used_logical_bytes": gorm.Expr("used_logical_bytes + ?", logical),
	}).Error
}

func (s *Service) AddLogicalUsage(tx *gorm.DB, userID uuid.UUID, size int64) error {
	result := tx.Model(&User{}).
		Where("id = ? AND used_logical_bytes + reserved_bytes + ? <= quota_bytes", userID, size).
		Update("used_logical_bytes", gorm.Expr("used_logical_bytes + ?", size))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Service) RemoveLogicalUsage(tx *gorm.DB, userID uuid.UUID, size int64) error {
	return tx.Model(&User{}).Where("id = ?", userID).
		Update("used_logical_bytes", gorm.Expr("GREATEST(used_logical_bytes - ?, 0)", size)).Error
}

func (s *Service) issueSession(db *gorm.DB, user User, familyID uuid.UUID) (Session, error) {
	accessToken, accessExpiry, err := s.tokens.AccessToken(user.ID)
	if err != nil {
		return Session{}, err
	}
	refreshToken, refreshHash, refreshExpiry, err := s.tokens.RefreshToken()
	if err != nil {
		return Session{}, err
	}
	refresh := RefreshSession{
		ID:        uuid.Must(uuid.NewV7()),
		UserID:    user.ID,
		TokenHash: refreshHash,
		FamilyID:  familyID,
		ExpiresAt: refreshExpiry,
	}
	if err := db.Create(&refresh).Error; err != nil {
		return Session{}, err
	}
	return Session{
		User:          user,
		AccessToken:   accessToken,
		AccessExpires: accessExpiry,
		RefreshToken:  refreshToken,
		RefreshExpiry: refreshExpiry,
	}, nil
}
