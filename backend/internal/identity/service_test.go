package identity

import (
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func identityFixture(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	cfg := &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatal("open test postgres: connection failed")
	}
	adminSQL, _ := admin.DB()
	schema := "identity_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Error(err)
		}
		_ = adminSQL.Close()
	})
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid test postgres URL")
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatal("open isolated schema: connection failed")
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = sqlDB.Close() })
	migration, err := os.ReadFile("../../migrations/000002_identity.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(string(migration)).Error; err != nil {
		t.Fatal(err)
	}
	tokens := NewTokenManager(config.Auth{Issuer: "identity-test", SigningSeed: "identity-test-seed", AccessTTL: time.Minute, RefreshTTL: time.Hour})
	return NewService(db, tokens, nil, 1024*1024), db
}

func TestRefreshReplayCommitsFamilyRevocation(t *testing.T) {
	s, db := identityFixture(t)
	initial, err := s.Register("refresh-user", "", "refresh-password")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := s.Rotate(initial.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rotate(initial.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replay error = %v", err)
	}
	if _, err := s.Rotate(rotated.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("descendant survived replay: %v", err)
	}
	var active int64
	if err := db.Model(&RefreshSession{}).Where("user_id = ? AND revoked_at IS NULL", initial.User.ID).Count(&active).Error; err != nil || active != 0 {
		t.Fatalf("active sessions = %d, error = %v", active, err)
	}
}

func TestRefreshReplaySerializesWithDescendantRotation(t *testing.T) {
	s, db := identityFixture(t)
	initial, err := s.Register("refresh-race", "", "refresh-password")
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		root, err := s.Login("refresh-race", "refresh-password")
		if err != nil {
			t.Fatal(err)
		}
		child, err := s.Rotate(root.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, token := range []string{root.RefreshToken, child.RefreshToken} {
			wg.Add(1)
			go func(raw string) { defer wg.Done(); <-start; _, err := s.Rotate(raw); errs <- err }(token)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && !errors.Is(err, ErrUnauthorized) {
				t.Fatal(err)
			}
		}
		var family RefreshSession
		if err := db.Where("token_hash = ?", RefreshHash(root.RefreshToken)).First(&family).Error; err != nil {
			t.Fatal(err)
		}
		var active int64
		if err := db.Model(&RefreshSession{}).Where("family_id = ? AND revoked_at IS NULL", family.FamilyID).Count(&active).Error; err != nil || active != 0 {
			t.Fatalf("descendant escaped revocation: count = %d, error = %v", active, err)
		}
	}
	if _, err := s.Rotate(initial.RefreshToken); err != nil {
		t.Fatalf("unrelated family revoked: %v", err)
	}
}

func TestRegisterRejectsCrossColumnLoginCollision(t *testing.T) {
	s, _ := identityFixture(t)
	if _, err := s.Register("existing@example.test", "", "first-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register("email-owner", " EXISTING@example.test ", "second-password"); !errors.Is(err, ErrConflict) {
		t.Fatalf("email collision error = %v", err)
	}
	if _, err := s.Register("first-owner", "other@example.test", "first-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register("OTHER@example.test", "", "second-password"); !errors.Is(err, ErrConflict) {
		t.Fatalf("username collision error = %v", err)
	}
	if _, err := s.Register("same@example.test", "same@example.test", "same-password"); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentRegistrationSharesLoginNamespace(t *testing.T) {
	s, db := identityFixture(t)
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() { <-start; _, err := s.Register("race@example.test", "", "first-password"); errs <- err }()
	go func() {
		<-start
		_, err := s.Register("email-owner", "race@example.test", "second-password")
		errs <- err
	}()
	close(start)
	passed, conflicts := 0, 0
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			passed++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if passed != 1 || conflicts != 1 {
		t.Fatalf("success = %d, conflicts = %d", passed, conflicts)
	}
	var count int64
	if err := db.Model(&User{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("users = %d, error = %v", count, err)
	}
}

func TestLoginSupportsLegacyCrossColumnCollision(t *testing.T) {
	s, db := identityFixture(t)
	first, err := s.Register("legacy@example.test", "", "first-password")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword("second-password")
	if err != nil {
		t.Fatal(err)
	}
	email := "legacy@example.test"
	second := User{ID: uuid.New(), Username: "second-owner", UsernameNormalized: "second-owner", EmailNormalized: &email, PasswordHash: hash, Status: UserStatusActive, QuotaBytes: 1024 * 1024}
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		password string
		id       uuid.UUID
	}{{"first-password", first.User.ID}, {"second-password", second.ID}} {
		session, err := s.Login(" LEGACY@example.test ", tc.password)
		if err != nil || session.User.ID != tc.id {
			t.Fatalf("wrong account selected: %s, error = %v", session.User.ID, err)
		}
	}
	if _, err := s.Login(email, "wrong-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid password error = %v", err)
	}
	if err := db.Model(&second).Update("password_hash", first.User.PasswordHash).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(email, "first-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ambiguous credentials error = %v", err)
	}
}
