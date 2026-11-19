package asyncjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
)

func TestEventRetentionRequiresAcceptedPosition(t *testing.T) {
	biz := testdb.Business(t)
	s := asynccontrol.New(testdb.Control(t))
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := outbox.Add(biz, "file", uuid.New(), outbox.EventFileCreated, map[string]string{"fixture": "retention"}); err != nil {
			t.Fatal(err)
		}
	}
	var rows []struct {
		ID  uuid.UUID
		XID string `gorm:"column:xid"`
		Seq int64
	}
	if err := biz.Raw("SELECT id,event_xid::text AS xid,seq FROM outbox_events ORDER BY event_xid,seq").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := biz.Exec("UPDATE outbox_events SET created_at=clock_timestamp()-interval '30 days'").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("INSERT INTO relay_positions(source,last_xid,last_seq) VALUES('business-primary',?::xid8,?)", rows[0].XID, rows[0].Seq).Error; err != nil {
		t.Fatal(err)
	}
	if err := MaintainEvents(ctx, biz, s, 10, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	if err := biz.Table("outbox_events").Pluck("id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != rows[1].ID {
		t.Fatal("retention deleted unaccepted event", ids)
	}
}
