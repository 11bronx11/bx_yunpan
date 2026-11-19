package asyncjobs

import (
	"context"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"gorm.io/gorm"
)

// MaintainEvents runs in the trusted business writer. Retention is constrained
// by the durably accepted transaction position and the recovery window; age
// alone never authorizes deleting an event from a late-committing transaction.
func MaintainEvents(ctx context.Context, business *gorm.DB, control *asynccontrol.Store, rows int, retention time.Duration) error {
	var p struct {
		XID string `gorm:"column:xid"`
		Seq int64
	}
	q := control.DB.WithContext(ctx).Raw(`SELECT last_xid::text AS xid,last_seq AS seq FROM relay_positions WHERE source='business-primary'`).Scan(&p)
	if q.Error != nil {
		return q.Error
	}
	if p.XID == "" {
		return nil
	}
	return business.WithContext(ctx).Exec(`DELETE FROM outbox_events WHERE id IN (
 SELECT id FROM outbox_events WHERE (event_xid,seq)<=(?::xid8,?) AND event_xid<pg_snapshot_xmin(pg_current_snapshot()) AND created_at<clock_timestamp()-(?*interval '1 millisecond') ORDER BY event_xid,seq LIMIT ?
 )`, p.XID, p.Seq, retention.Milliseconds(), rows).Error
}
