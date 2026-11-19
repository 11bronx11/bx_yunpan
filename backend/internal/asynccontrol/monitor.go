package asynccontrol

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Monitor exposes process liveness separately from dependency readiness and
// module ownership. A standby module is healthy; ownership is a separate gauge.
func (s *Store) Monitor(ctx context.Context, addr, holder string, business *gorm.DB, broker *Broker, logger *slog.Logger) (func(), error) {
	if addr == "" {
		return func() {}, nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		bounded, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var one int
		err := s.critical().WithContext(bounded).Raw("SELECT 1").Scan(&one).Error
		if err == nil {
			err = business.WithContext(bounded).Raw("SELECT 1").Scan(&one).Error
		}
		if err == nil {
			err = broker.Ready(bounded)
		}
		if err != nil {
			http.Error(w, "dependencies unavailable", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		bounded, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "bx_async_commit_calls_total %d\nbx_async_commit_seconds_sum %g\nbx_async_receipt_hits_total %d\n", s.CommitCalls.Load(), float64(s.CommitNanos.Load())/float64(time.Second), s.ReceiptHits.Load())
		if holder != "async-control" && !strings.HasPrefix(holder, "control:") {
			return
		}
		var groups []struct {
			Queue, State      string
			Count             int64
			Age               float64
			Attempts, Commits int64
		}
		if err := s.DB.WithContext(bounded).Raw(`SELECT queue,state,count(*) AS count,EXTRACT(EPOCH FROM clock_timestamp()-min(updated_at)) AS age,sum(attempts) AS attempts,sum(commit_attempts) AS commits FROM tasks WHERE protocol_version=2 GROUP BY queue,state`).Scan(&groups).Error; err != nil {
			http.Error(w, "control unavailable", 503)
			return
		}
		for _, g := range groups {
			labels := fmt.Sprintf("queue=%s,state=%s", strconv.Quote(g.Queue), strconv.Quote(g.State))
			fmt.Fprintf(w, "bx_async_tasks{%s} %d\nbx_async_oldest_age_seconds{%s} %g\nbx_async_execution_starts{%s} %d\nbx_async_commit_attempts{%s} %d\n", labels, g.Count, labels, g.Age, labels, g.Attempts, labels, g.Commits)
		}
		var modules []struct {
			Name, Holder string
			Live         bool
		}
		if err := s.DB.WithContext(bounded).Raw("SELECT name,holder,lease_until>clock_timestamp() AS live FROM module_leases").Scan(&modules).Error; err == nil {
			for _, m := range modules {
				v := 0
				if m.Live {
					v = 1
				}
				fmt.Fprintf(w, "bx_async_module_active{module=%s,holder=%s} %d\n", strconv.Quote(m.Name), strconv.Quote(m.Holder), v)
			}
		}
		var p Position
		if err := s.DB.WithContext(bounded).Raw("SELECT source,last_xid::text AS last_x_id,last_seq,updated_at FROM relay_positions WHERE source='business-primary'").Scan(&p).Error; err == nil && p.LastXID != "" {
			fmt.Fprintf(w, "bx_async_relay_seq %d\nbx_async_relay_position_age_seconds %g\n", p.LastSeq, time.Since(p.UpdatedAt).Seconds())
			var oldest struct {
				Age   float64
				Count int64
			}
			if err := business.WithContext(bounded).Raw(`SELECT COALESCE(EXTRACT(EPOCH FROM clock_timestamp()-min(created_at)),0) AS age,count(*) AS count FROM (SELECT created_at FROM outbox_events WHERE (event_xid,seq)>(?::xid8,?) ORDER BY event_xid,seq LIMIT 2000) sample`, p.LastXID, p.LastSeq).Scan(&oldest).Error; err == nil {
				fmt.Fprintf(w, "bx_async_relay_unaccepted_sample %d\nbx_async_relay_oldest_sample_seconds %g\n", oldest.Count, oldest.Age)
			}
			var xmin string
			if err := business.WithContext(bounded).Raw("SELECT pg_snapshot_xmin(pg_current_snapshot())::text").Scan(&xmin).Error; err == nil {
				fmt.Fprintf(w, "bx_async_relay_safe_xmin %s\nbx_async_relay_last_xid %s\n", xmin, p.LastXID)
			}
		}
		var quarantines int64
		if err := s.DB.WithContext(bounded).Table("quarantines").Count(&quarantines).Error; err == nil {
			fmt.Fprintf(w, "bx_async_quarantines %d\n", quarantines)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("async monitor stopped", "error", err, "holder", holder)
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	return func() { stop(); _ = server.Close() }, nil
}
