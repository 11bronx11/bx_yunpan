package upload

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

func TestAuditIdempotencyBindsUploadParameters(t *testing.T) {
	f := newConcurrencyFixture(t)
	in := CreateInput{FolderID: f.rootID, Filename: "audit.txt", SHA256: strings.Repeat("a", 64), SizeBytes: 128, MimeType: "text/plain", IdempotencyKey: uuid.NewString()}
	first, err := f.service.Create(context.Background(), f.userID, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CreateInput){
		func(i *CreateInput) { i.Filename = "AUDIT.txt" },
		func(i *CreateInput) { i.SHA256 = strings.Repeat("b", 64) },
		func(i *CreateInput) { i.SizeBytes++ },
		func(i *CreateInput) { i.MimeType = "application/pdf" },
	} {
		changed := in
		mutate(&changed)
		if _, err := f.service.Create(context.Background(), f.userID, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed request error=%v", err)
		}
	}
	again, err := f.service.Create(context.Background(), f.userID, in)
	if err != nil || again.Upload.ID != first.Upload.ID {
		t.Fatalf("retry=%+v error=%v", again, err)
	}
}

func TestAuditObjectCanBeCollectedAndReuploadedTwice(t *testing.T) {
	f := newConcurrencyFixture(t)
	store := newFakeUploadStorage()
	f.service.storage = store
	content := []byte("audit generation " + uuid.NewString())
	var objectID uuid.UUID
	var keys []string
	for i := 0; i < 3; i++ {
		session := newVerifyingSession(f.userID, f.rootID, fmt.Sprintf("generation-%d.txt", i), content)
		if err := f.db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.db.Model(&identity.User{}).Where("id = ?", f.userID).Update("reserved_bytes", session.ReservedBytes).Error; err != nil {
			t.Fatal(err)
		}
		store.put(session.Bucket, session.ObjectKey, content)
		if err := f.service.Verify(context.Background(), session.ID); err != nil {
			t.Fatal(err)
		}
		var object objectstore.Object
		if err := f.db.Where("sha256 = ?", session.DeclaredSHA256).First(&object).Error; err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			objectID = object.ID
		}
		if object.ID != objectID || object.ReferenceCount != 1 || object.DeletedAt != nil || !store.exists(object.Bucket, object.ObjectKey) {
			t.Fatalf("object generation %d = %+v", i, object)
		}
		for _, old := range keys {
			if old == object.ObjectKey {
				t.Fatal("reused physical generation")
			}
		}
		keys = append(keys, object.ObjectKey)
		if err := f.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("UPDATE file_entries SET deleted_at = now() WHERE object_id = ?", object.ID).Error; err != nil {
				return err
			}
			_, err := f.service.objects.ReleaseReference(tx, object.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.service.objects.GarbageCollect(context.Background(), store, object.ID); err != nil {
			t.Fatal(err)
		}
		if store.exists(object.Bucket, object.ObjectKey) {
			t.Fatal("GC retained bytes")
		}
		if err := f.db.First(&object, "id = ?", object.ID).Error; err != nil || object.Status != "deleted" {
			t.Fatalf("GC state=%s error=%v", object.Status, err)
		}
	}
}

func TestAuditVerificationCleanupIsAtomic(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			f := newConcurrencyFixture(t)
			store := newFakeUploadStorage()
			f.service.storage = store
			content := []byte("cleanup " + uuid.NewString())
			s := newVerifyingSession(f.userID, f.rootID, "cleanup.txt", content)
			if mismatch {
				s.DeclaredSHA256 = strings.Repeat("f", 64)
			}
			if err := f.db.Create(&s).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Model(&identity.User{}).Where("id = ?", f.userID).Update("reserved_bytes", s.ReservedBytes).Error; err != nil {
				t.Fatal(err)
			}
			store.put(s.Bucket, s.ObjectKey, content)
			name := "audit_cleanup_" + strings.ReplaceAll(s.ID.String(), "-", "")
			sql := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.upload_session_id = '%s' AND NEW.operation_type = 'staging.delete' THEN RAISE EXCEPTION 'audit cleanup write failure'; END IF; RETURN NEW; END $$`, name, s.ID)
			if err := f.db.Exec(sql).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.db.Exec("DROP FUNCTION " + name + "() CASCADE").Error })
			if err := f.db.Exec(fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT ON storage_operations FOR EACH ROW EXECUTE FUNCTION %s()", name, name)).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.service.Verify(context.Background(), s.ID); err == nil {
				t.Fatal("cleanup write failure committed verification")
			}
			got, err := f.service.Get(f.userID, s.ID)
			if err != nil || got.Status != StatusVerifying || got.ReservedBytes != s.ReservedBytes {
				t.Fatalf("partial commit=%+v error=%v", got, err)
			}
			if err := f.db.Exec("DROP TRIGGER " + name + " ON storage_operations").Error; err != nil {
				t.Fatal(err)
			}
			if err := f.service.Verify(context.Background(), s.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type auditAbortDuringCreate struct {
	*fakeMultipartStorage
	cancel func()
}

func (s *auditAbortDuringCreate) NewMultipartUpload(ctx context.Context, b, k string, opts minio.PutObjectOptions) (string, error) {
	s.cancel()
	return s.fakeMultipartStorage.NewMultipartUpload(ctx, b, k, opts)
}

func TestAuditAbortDuringProviderCreateIsReconciled(t *testing.T) {
	f := newConcurrencyFixture(t)
	result, err := f.service.Create(context.Background(), f.userID, CreateInput{FolderID: f.rootID, Filename: "cancel.txt", SHA256: strings.Repeat("a", 64), SizeBytes: 128, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	s := *result.Upload
	f.service.core = &auditAbortDuringCreate{fakeMultipartStorage: f.storage, cancel: func() {
		if err := f.service.Abort(context.Background(), f.userID, s.ID); err != nil {
			t.Fatal(err)
		}
	}}
	var create storageops.Operation
	if err := f.db.Where("upload_session_id = ? AND operation_type = ?", s.ID, storageops.MultipartCreate).First(&create).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileStorageOperation(context.Background(), create.ID); err != nil {
		t.Fatal(err)
	}
	var abort storageops.Operation
	if err := f.db.Where("upload_session_id = ? AND operation_type = ?", s.ID, storageops.MultipartAbort).First(&abort).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileStorageOperation(context.Background(), abort.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.service.Get(f.userID, s.ID)
	if err != nil || got.Status != StatusAborted || got.ReservedBytes != 0 || f.storage.abortCount() != 1 {
		t.Fatalf("cancel result=%+v error=%v", got, err)
	}
}
