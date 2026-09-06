package ownerepoch

import (
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func epochDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestEpochBumpsAreAtomicAndRollbackSafe(t *testing.T) {
	db := epochDB(t)
	ownerID := uuid.New()
	if err := db.Exec(`INSERT INTO users (id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'test')`, ownerID, "epoch-"+ownerID.String(), "epoch-"+ownerID.String()).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Exec("DELETE FROM users WHERE id = ?", ownerID).Error })
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := Ensure(tx, ownerID); err != nil {
			return err
		}
		epoch, err := BumpContent(tx, ownerID)
		if err != nil || epoch.ContentEpoch != 2 || epoch.ACLEpoch != 1 {
			return errors.New("unexpected content epoch")
		}
		epoch, err = BumpACL(tx, ownerID)
		if err != nil || epoch.ContentEpoch != 2 || epoch.ACLEpoch != 2 {
			return errors.New("unexpected acl epoch")
		}
		return errors.New("rollback epoch probe")
	}); err == nil || err.Error() != "rollback epoch probe" {
		t.Fatalf("transaction result = %v", err)
	}
	var count int64
	if err := db.Model(&Epoch{}).Where("owner_id = ?", ownerID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled back epoch rows = %d", count)
	}
}
