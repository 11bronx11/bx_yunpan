package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

// Exercise the actual planner and receipt transaction, using only business PG.
// Compute outputs are injected; this does not exercise control scheduling or S3.
type businessUploadFixture struct {
	c             *Committer
	owner, folder uuid.UUID
}

func newBusinessUpload(t *testing.T) businessUploadFixture {
	t.Helper()
	db := testdb.Business(t)
	owner := uuid.New()
	if err := db.Create(&identity.User{ID: owner, Username: owner.String(), UsernameNormalized: owner.String(), PasswordHash: "test", Status: 1, QuotaBytes: 1 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	d := drive.NewService(db)
	if err := d.ProvisionUser(db, owner); err != nil {
		t.Fatal(err)
	}
	root, err := d.Root(owner)
	if err != nil {
		t.Fatal(err)
	}
	objects := objectstore.NewService(db)
	service := upload.NewService(db, nil, nil, "test", d, objects, identity.NewService(db, nil, d, 1<<30), config.Upload{SessionTTL: time.Hour})
	return businessUploadFixture{c: &Committer{Business: db, Control: &asynccontrol.Store{}, Upload: service, Objects: objects}, owner: owner, folder: root.ID}
}

func (f businessUploadFixture) create(t *testing.T, name string) upload.Session {
	t.Helper()
	r, err := f.c.Upload.Create(context.Background(), f.owner, upload.CreateInput{FolderID: f.folder, Filename: name, SHA256: strings.Repeat("a", 64), SizeBytes: 128, MimeType: "application/octet-stream", IdempotencyKey: uuid.NewString()})
	if err != nil || r.Upload == nil {
		t.Fatalf("create: %+v %v", r, err)
	}
	return *r.Upload
}

func (f businessUploadFixture) session(t *testing.T, id uuid.UUID) upload.Session {
	t.Helper()
	s, err := f.c.Upload.Get(f.owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f businessUploadFixture) count(t *testing.T, table, where string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.c.Business.Table(table).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func (f businessUploadFixture) occupied(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.c.Business.Table("user_quota_usage").Where("owner_id=?", f.owner).Pluck("occupied_bytes", &n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

type businessUploadResult struct {
	task   asynccontrol.Task
	result asynccontrol.Result
	in     asyncprotocol.Input
	out    asyncprotocol.Output
}

func (f businessUploadFixture) planned(t *testing.T, sessionID uuid.UUID, kind string, out asyncprotocol.Output) businessUploadResult {
	t.Helper()
	var event asynccontrol.Event
	if kind == "upload.verify" || kind == "upload.expire" {
		eventType := eventcontract.EventObjectVerifyRequestedV1
		if kind == "upload.expire" {
			eventType = eventcontract.EventUploadExpiryRequestedV1
		}
		if err := f.c.Business.Table("outbox_events").Where("aggregate_id=? AND event_type=?", sessionID, eventType).Take(&event).Error; err != nil {
			t.Fatal(err)
		}
	} else {
		var op storageops.Operation
		if err := f.c.Business.Where("upload_session_id=? AND operation_type=?", sessionID, kind).Take(&op).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.c.Business.Table("outbox_events").Where("aggregate_id=?", op.ID).Take(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	specs, err := (&Planner{Read: f.c.Business}).Plan(context.Background(), event)
	if err != nil || len(specs) != 1 || specs[0].Kind != kind {
		t.Fatalf("plan %s: %+v %v", kind, specs, err)
	}
	x := businessUploadResult{task: asynccontrol.Task{ID: uuid.New(), Kind: kind, SemanticKey: specs[0].SemanticKey}}
	if err := json.Unmarshal(specs[0].Input, &x.in); err != nil {
		t.Fatal(err)
	}
	x.result.InputDigest, _, err = asynccontrol.Digest(specs[0].Input)
	if err != nil {
		t.Fatal(err)
	}
	return x.withOutput(t, out)
}

func (x businessUploadResult) withOutput(t *testing.T, out asyncprotocol.Output) businessUploadResult {
	t.Helper()
	x.out = out
	x.result.ID = uuid.New()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	x.result.ResultDigest, _, err = asynccontrol.Digest(b)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (f businessUploadFixture) commit(x businessUploadResult) (asyncprotocol.Decision, error) {
	var decision asyncprotocol.Decision
	err := f.c.Business.Transaction(func(tx *gorm.DB) error {
		var err error
		decision, err = f.c.commitTx(tx, x.task, x.result, x.in, x.out)
		return err
	})
	return decision, err
}

func (f businessUploadFixture) requireCommit(t *testing.T, x businessUploadResult) asyncprotocol.Decision {
	t.Helper()
	d, err := f.commit(x)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f businessUploadFixture) verifying(t *testing.T, s upload.Session) upload.Session {
	t.Helper()
	f.requireCommit(t, f.planned(t, s.ID, storageops.MultipartCreate, asyncprotocol.Output{UploadID: "s3-" + s.ID.String()}))
	if _, err := f.c.Upload.ConfirmPartsContext(context.Background(), f.owner, s.ID, []upload.ConfirmedPart{{PartNumber: 1, SizeBytes: 128, ETag: "original"}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Upload.Complete(context.Background(), f.owner, s.ID); err != nil {
		t.Fatal(err)
	}
	x := f.planned(t, s.ID, storageops.MultipartComplete, asyncprotocol.Output{})
	if len(x.in.Parts) != 1 || x.in.Parts[0].Number != 1 || x.in.Parts[0].ETag != "original" {
		t.Fatal("planner lost frozen merge input")
	}
	if _, err := f.c.Upload.ConfirmPartsContext(context.Background(), f.owner, s.ID, []upload.ConfirmedPart{{PartNumber: 1, SizeBytes: 128, ETag: "other"}}, true); !errors.Is(err, upload.ErrPartConflict) {
		t.Fatalf("frozen merge overwritten: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.commit(x); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.count(t, "async_commit_receipts", "semantic_key=?", x.task.SemanticKey) != 1 || f.count(t, "outbox_events", "aggregate_id=? AND event_type=?", s.ID, eventcontract.EventObjectVerifyRequestedV1) != 1 {
		t.Fatal("merge replay duplicated receipt or verify event")
	}
	return f.session(t, s.ID)
}

func TestUploadBusinessPublicationReceiptRollbackAndReplay(t *testing.T) {
	f := newBusinessUpload(t)
	s := f.verifying(t, f.create(t, "publish.bin"))
	f.requireCommit(t, f.planned(t, s.ID, "upload.verify", asyncprotocol.Output{SHA256: s.DeclaredSHA256, Size: s.SizeBytes}))
	x := f.planned(t, s.ID, storageops.CanonicalEnsure, asyncprotocol.Output{})
	x = x.withOutput(t, asyncprotocol.Output{Bucket: x.in.TargetBucket, Key: x.in.TargetKey + "generation-1"})
	if err := f.c.Business.Exec(`CREATE FUNCTION reject_upload_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.semantic_key LIKE 'canonical.ensure:%' THEN RAISE EXCEPTION 'injected receipt failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_upload_receipt BEFORE INSERT ON async_commit_receipts FOR EACH ROW EXECUTE FUNCTION reject_upload_receipt()`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.commit(x); err == nil {
		t.Fatal("receipt fault did not roll back")
	}
	if f.session(t, s.ID).Status != upload.StatusVerifying || f.count(t, "file_objects", "true") != 0 || f.count(t, "file_entries", "true") != 0 || f.occupied(t) != 128 {
		t.Fatal("publication escaped failed receipt transaction")
	}
	if err := f.c.Business.Exec("DROP TRIGGER reject_upload_receipt ON async_commit_receipts").Error; err != nil {
		t.Fatal(err)
	}
	f.requireCommit(t, x)
	for range 4 {
		f.requireCommit(t, x)
	}
	got := f.session(t, s.ID)
	if got.Status != upload.StatusCompleted || got.ReservedBytes != 0 || got.CompletedEntryID == nil || got.CompletedObjectID == nil || f.occupied(t) != 128 {
		t.Fatal("publication settlement", got)
	}
	var object objectstore.Object
	if err := f.c.Business.First(&object, "id=?", *got.CompletedObjectID).Error; err != nil {
		t.Fatal(err)
	}
	if object.ReferenceCount != 1 || f.count(t, "file_entries", "deleted_at IS NULL") != 1 || f.count(t, "async_commit_receipts", "semantic_key=?", x.task.SemanticKey) != 1 {
		t.Fatal("replayed publication duplicated business effects")
	}
	if f.count(t, "storage_operations", "source_key=? AND operation_type=?", object.ObjectKey, storageops.StagingDelete) != 0 {
		t.Fatal("duplicate result scheduled deletion of active object")
	}
	conflict := x.withOutput(t, asyncprotocol.Output{Bucket: x.out.Bucket, Key: x.out.Key + "-different"})
	if _, err := f.commit(conflict); !errors.Is(err, asynccontrol.ErrConflict) {
		t.Fatalf("conflicting relay receipt: %v", err)
	}
}

func TestUploadBusinessExhaustionAndLateCreate(t *testing.T) {
	for _, state := range []string{upload.StatusInitializing, upload.StatusCompleting} {
		t.Run(state, func(t *testing.T) {
			f := newBusinessUpload(t)
			s := f.create(t, "exhausted.bin")
			create := f.planned(t, s.ID, storageops.MultipartCreate, asyncprotocol.Output{UploadID: "late-s3"})
			x := create
			if state == upload.StatusCompleting {
				f.requireCommit(t, create)
				if _, err := f.c.Upload.ConfirmParts(f.owner, s.ID, []upload.ConfirmedPart{{PartNumber: 1, SizeBytes: 128, ETag: "one"}}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.c.Upload.Complete(context.Background(), f.owner, s.ID); err != nil {
					t.Fatal(err)
				}
				x = f.planned(t, s.ID, storageops.MultipartComplete, asyncprotocol.Output{})
			}
			x = x.withOutput(t, asyncprotocol.Output{Convergence: true, Error: strings.Repeat("network unavailable ", 50)})
			for range 4 {
				f.requireCommit(t, x)
			}
			got := f.session(t, s.ID)
			if got.Status != upload.StatusFailed || got.ErrorCode == nil || *got.ErrorCode != "upload.async_exhausted" || got.ReservedBytes != 0 || f.occupied(t) != 0 {
				t.Fatal("exhaustion did not settle once", got)
			}
			if _, err := f.c.Upload.ConfirmParts(f.owner, s.ID, []upload.ConfirmedPart{{PartNumber: 1, SizeBytes: 128, ETag: "one"}}); !errors.Is(err, upload.ErrConflict) {
				t.Fatal("failed session reopened", err)
			}
			// A distinct, delayed create result may arrive after cancellation/failure.
			late := create
			late.task.SemanticKey += "-late"
			late = late.withOutput(t, asyncprotocol.Output{UploadID: "orphan-s3"})
			if d := f.requireCommit(t, late); d.Outcome != "superseded" {
				t.Fatal(d)
			}
			if f.session(t, s.ID).Status != upload.StatusFailed || f.count(t, "storage_operations", "upload_session_id=? AND storage_upload_id=? AND operation_type=?", s.ID, "orphan-s3", storageops.MultipartAbort) != 1 {
				t.Fatal("late create revived session or leaked multipart")
			}
		})
	}
}

func TestUploadBusinessInvalidResultAndHashMismatch(t *testing.T) {
	f := newBusinessUpload(t)
	s := f.verifying(t, f.create(t, "mismatch.bin"))
	x := f.planned(t, s.ID, "upload.verify", asyncprotocol.Output{SHA256: s.DeclaredSHA256, Size: s.SizeBytes})
	wrongOwner := x
	wrongOwner.in.OwnerID = uuid.New()
	if _, err := f.commit(wrongOwner); !errors.Is(err, upload.ErrInvalidInput) {
		t.Fatal("wrong owner accepted", err)
	}
	if f.session(t, s.ID).Status != upload.StatusVerifying || f.count(t, "async_commit_receipts", "semantic_key=?", x.task.SemanticKey) != 0 {
		t.Fatal("invalid result changed facts")
	}
	x = x.withOutput(t, asyncprotocol.Output{SHA256: strings.Repeat("b", 64), Size: s.SizeBytes})
	if d := f.requireCommit(t, x); d.Outcome != "rejected" {
		t.Fatal(d)
	}
	got := f.session(t, s.ID)
	if got.Status != upload.StatusFailed || got.ErrorCode == nil || *got.ErrorCode != "upload.hash_mismatch" || f.occupied(t) != 0 || f.count(t, "file_entries", "true") != 0 {
		t.Fatal("bad content published", got)
	}
}

func TestUploadBusinessCompetingContentConvergesAndDuplicateInstantIsRejected(t *testing.T) {
	f := newBusinessUpload(t)
	first := f.verifying(t, f.create(t, "winner.bin"))
	second := f.verifying(t, f.create(t, "contender.bin"))
	f.requireCommit(t, f.planned(t, first.ID, "upload.verify", asyncprotocol.Output{SHA256: first.DeclaredSHA256, Size: first.SizeBytes}))
	x := f.planned(t, first.ID, storageops.CanonicalEnsure, asyncprotocol.Output{})
	x = x.withOutput(t, asyncprotocol.Output{Bucket: x.in.TargetBucket, Key: x.in.TargetKey + "winner"})
	f.requireCommit(t, x)
	// The competing session now finds the object ready, so no copy is needed.
	verify := f.planned(t, second.ID, "upload.verify", asyncprotocol.Output{SHA256: second.DeclaredSHA256, Size: second.SizeBytes})
	if _, err := f.commit(verify); !errors.Is(err, upload.ErrNameConflict) {
		t.Fatalf("duplicate owned content: %v", err)
	}
	// This is the same convergence output selected by Commit after that error.
	verify = verify.withOutput(t, asyncprotocol.Output{Convergence: true, Error: "upload.commit_precondition_failed"})
	f.requireCommit(t, verify)
	if f.session(t, second.ID).Status != upload.StatusFailed || f.occupied(t) != 128 || f.count(t, "file_entries", "deleted_at IS NULL") != 1 || f.count(t, "file_objects", "reference_count=1") != 1 {
		t.Fatal("content competition leaked reservation or reference")
	}
	_, err := f.c.Upload.Create(context.Background(), f.owner, upload.CreateInput{FolderID: f.folder, Filename: "same-content-another-name.bin", SHA256: first.DeclaredSHA256, SizeBytes: 128, MimeType: first.MimeType, IdempotencyKey: uuid.NewString()})
	if !errors.Is(err, upload.ErrFileExists) {
		t.Fatalf("existing owned content should follow current dedup rule: %v", err)
	}
	if f.count(t, "upload_sessions", "true") != 2 || f.occupied(t) != 128 {
		t.Fatal("rejected instant request left facts")
	}
}

func TestUploadBusinessCrossOwnerHashRequiresUploadThenReusesObject(t *testing.T) {
	f := newBusinessUpload(t)
	first := f.verifying(t, f.create(t, "original.bin"))
	f.requireCommit(t, f.planned(t, first.ID, "upload.verify", asyncprotocol.Output{SHA256: first.DeclaredSHA256, Size: first.SizeBytes}))
	x := f.planned(t, first.ID, storageops.CanonicalEnsure, asyncprotocol.Output{})
	x = x.withOutput(t, asyncprotocol.Output{Bucket: x.in.TargetBucket, Key: x.in.TargetKey + "original"})
	f.requireCommit(t, x)
	other := f
	other.owner = uuid.New()
	if err := f.c.Business.Create(&identity.User{ID: other.owner, Username: other.owner.String(), UsernameNormalized: other.owner.String(), PasswordHash: "test", Status: 1, QuotaBytes: 1 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	driveService := drive.NewService(f.c.Business)
	if err := driveService.ProvisionUser(f.c.Business, other.owner); err != nil {
		t.Fatal(err)
	}
	root, err := driveService.Root(other.owner)
	if err != nil {
		t.Fatal(err)
	}
	other.folder = root.ID
	second := other.create(t, "unproven-hash.bin")
	if second.Status != upload.StatusInitializing {
		t.Fatal("a hash declaration bypassed proof of content")
	}
	second = other.verifying(t, second)
	other.requireCommit(t, other.planned(t, second.ID, "upload.verify", asyncprotocol.Output{SHA256: second.DeclaredSHA256, Size: second.SizeBytes}))
	if other.session(t, second.ID).Status != upload.StatusCompleted || other.occupied(t) != 128 || f.count(t, "file_objects", "reference_count=2") != 1 || f.count(t, "storage_operations", "upload_session_id=? AND operation_type=?", second.ID, storageops.CanonicalEnsure) != 0 {
		t.Fatal("verified content did not reuse ready object")
	}
}

func TestUploadBusinessExpiryReschedulesThenCleansUnknownCreate(t *testing.T) {
	f := newBusinessUpload(t)
	s := f.create(t, "expire-before-create.bin")
	s = f.session(t, s.ID)
	x := f.planned(t, s.ID, "upload.expire", asyncprotocol.Output{})
	decision := f.requireCommit(t, x)
	if decision.RescheduleAt == nil || !decision.RescheduleAt.Equal(s.ExpiresAt) || f.count(t, "async_commit_receipts", "semantic_key=?", x.task.SemanticKey) != 0 || f.occupied(t) != 128 {
		t.Fatal("early expiry consumed receipt or reservation", decision)
	}
	if err := f.c.Business.Model(&upload.Session{}).Where("id=?", s.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	for range 4 {
		f.requireCommit(t, x)
	}
	if got := f.session(t, s.ID); got.Status != upload.StatusExpired || got.ReservedBytes != 0 || f.occupied(t) != 0 {
		t.Fatal("expiry failed", got)
	}
	for _, kind := range []string{"multipart.sweep", storageops.StagingDelete} {
		if f.count(t, "storage_operations", "upload_session_id=? AND operation_type=?", s.ID, kind) != 1 {
			t.Fatalf("expiry missed %s", kind)
		}
	}
	late := f.planned(t, s.ID, storageops.MultipartCreate, asyncprotocol.Output{UploadID: "late-after-expiry"})
	if d := f.requireCommit(t, late); d.Outcome != "superseded" {
		t.Fatal(d)
	}
	if f.session(t, s.ID).Status != upload.StatusExpired || f.count(t, "storage_operations", "upload_session_id=? AND operation_type=? AND storage_upload_id=?", s.ID, storageops.MultipartAbort, "late-after-expiry") != 1 {
		t.Fatal("expired unknown create leaked or reopened")
	}
}

func TestUploadBusinessCanonicalResultMustMatchPrivateDestination(t *testing.T) {
	f := newBusinessUpload(t)
	s := f.verifying(t, f.create(t, "wrong-destination.bin"))
	f.requireCommit(t, f.planned(t, s.ID, "upload.verify", asyncprotocol.Output{SHA256: s.DeclaredSHA256, Size: s.SizeBytes}))
	x := f.planned(t, s.ID, storageops.CanonicalEnsure, asyncprotocol.Output{})
	for _, out := range []asyncprotocol.Output{
		{Bucket: "other-bucket", Key: x.in.TargetKey + "generation"},
		{Bucket: x.in.TargetBucket, Key: "objects/compute/other-operation/generation"},
		{Bucket: x.in.TargetBucket, Key: x.in.TargetKey},
	} {
		if _, err := f.commit(x.withOutput(t, out)); !errors.Is(err, upload.ErrInvalidInput) {
			t.Fatalf("bad destination accepted: %+v %v", out, err)
		}
		if f.session(t, s.ID).Status != upload.StatusVerifying || f.count(t, "file_entries", "true") != 0 || f.count(t, "async_commit_receipts", "semantic_key=?", x.task.SemanticKey) != 0 || f.occupied(t) != 128 {
			t.Fatal("rejected relay mutated publication")
		}
	}
}
