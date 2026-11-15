package asynccontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

type ExternalOutput struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

func (s *Store) RecordOutput(ctx context.Context, t Task, bucket, key string) error {
	if t.Token == nil {
		return ErrLost
	}
	b, _ := json.Marshal(ExternalOutput{bucket, key})
	return owned(s.DB.WithContext(ctx).Exec(`UPDATE executions e SET outputs=CASE WHEN outputs @> jsonb_build_array(?::jsonb) THEN outputs ELSE outputs||jsonb_build_array(?::jsonb) END
 FROM tasks t WHERE e.task_id=t.id AND e.generation=t.generation AND t.id=? AND t.token=? AND t.generation=? AND t.state='running' AND t.lease_until>clock_timestamp()`, string(b), string(b), t.ID, *t.Token, t.Generation))
}

// RecoverOutputs scans unfinished execution output records only, never business
// history or a bucket. Delay covers bounded late external requests. Published
// winners are checked against business facts by the cleanup worker.
func (s *Store) RecoverOutputs(ctx context.Context, limit int, safetyDelay time.Duration) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []struct {
			TaskID     string
			Generation int64
			Outputs    json.RawMessage
			FinishedAt time.Time
		}
		if err := tx.Raw(`SELECT e.task_id::text,e.generation,e.outputs,e.finished_at FROM executions e
   WHERE e.finished_at<clock_timestamp()-(?*interval '1 millisecond') AND e.outcome IN ('lease_lost','retry_wait','exhausted') AND e.outputs<>'[]'::jsonb
   ORDER BY e.finished_at,e.task_id LIMIT ? FOR UPDATE SKIP LOCKED`, safetyDelay.Milliseconds(), limit).Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			var outputs []ExternalOutput
			if err := json.Unmarshal(row.Outputs, &outputs); err != nil {
				return err
			}
			for _, out := range outputs {
				input, _ := json.Marshal(out)
				spec := Spec{Kind: "output.delete", SemanticKey: fmt.Sprintf("output.delete:%s:%d:%s", row.TaskID, row.Generation, out.Key), ResourceKey: "output:" + row.TaskID, ShardKey: "output:" + row.TaskID, Input: input}
				if _, err := s.ensure(tx, spec, "execution:"+row.TaskID); err != nil {
					return err
				}
			}
			if err := tx.Exec("UPDATE executions SET outcome='cleanup_scheduled' WHERE task_id=?::uuid AND generation=?", row.TaskID, row.Generation).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
