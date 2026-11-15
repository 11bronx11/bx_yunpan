package dependencies

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

type AsyncSet struct {
	Control, Critical, Read, Business *gorm.DB
	close                             []func()
}

func (a *AsyncSet) Close() {
	for i := len(a.close) - 1; i >= 0; i-- {
		a.close[i]()
	}
}

func OpenAsync(ctx context.Context, cfg config.Config, write bool) (*AsyncSet, error) {
	a := &AsyncSet{}
	open := func(dsn string, limit, idle int) (*gorm.DB, error) {
		c := cfg
		c.Postgres.PrimaryDSN = dsn
		c.Postgres.DSN = dsn
		c.Postgres.MaxOpenConns = limit
		c.Postgres.MaxIdleConns = min(idle, limit)
		c.Postgres.ConnMaxLifetime = 30 * time.Minute
		c.Postgres.ConnMaxIdleTime = 5 * time.Minute
		db, sql, err := openPostgres(ctx, c)
		if err != nil {
			return nil, err
		}
		a.close = append(a.close, func() { _ = sql.Close() })
		return db, nil
	}
	fail := func(err error) (*AsyncSet, error) { a.Close(); return nil, err }
	var err error
	if a.Control, err = open(cfg.Async.ControlDSN, cfg.Async.ControlMaxOpen, cfg.Async.ControlMaxIdle); err != nil {
		return fail(err)
	}
	if a.Critical, err = open(cfg.Async.ControlDSN, cfg.Async.CriticalMaxOpen, 1); err != nil {
		return fail(err)
	}
	readDSN, err := readOnlyDSN(cfg.Async.BusinessReadDSN)
	if err != nil {
		return fail(err)
	}
	if a.Read, err = open(readDSN, max(2, cfg.Postgres.MaxOpenConns/2), 2); err != nil {
		return fail(err)
	}
	var writable bool
	if err := a.Read.Raw("SELECT has_table_privilege(current_user,'public.file_entries','INSERT,UPDATE,DELETE') OR has_table_privilege(current_user,'public.upload_sessions','INSERT,UPDATE,DELETE')").Scan(&writable).Error; err != nil {
		return fail(err)
	}
	if writable {
		return fail(errors.New("POSTGRES_READONLY_DSN role must not have business write privileges"))
	}
	var controlIdentity, businessIdentity string
	identity := `SELECT current_database()||':'||COALESCE(inet_server_addr()::text,'local')||':'||COALESCE(inet_server_port()::text,'local')`
	// Connections must name distinct databases even on the same cluster. Deploy
	// supports separate hosts; the readonly DSN must point to the business primary.
	if err := a.Control.Raw(identity).Scan(&controlIdentity).Error; err != nil {
		return fail(err)
	}
	if err := a.Read.Raw(identity).Scan(&businessIdentity).Error; err != nil {
		return fail(err)
	}
	if controlIdentity == businessIdentity {
		return fail(errors.New("business and control pools resolve to the same database"))
	}
	if write {
		if a.Business, err = open(cfg.Postgres.PrimaryDSN, cfg.Postgres.MaxOpenConns, cfg.Postgres.MaxIdleConns); err != nil {
			return fail(err)
		}
	}
	return a, nil
}

func readOnlyDSN(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("invalid readonly DSN")
		}
		q := u.Query()
		q.Set("default_transaction_read_only", "on")
		q.Set("statement_timeout", "5000")
		q.Set("connect_timeout", "5")
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	if strings.TrimSpace(dsn) == "" {
		return "", errors.New("readonly DSN required")
	}
	return dsn + " default_transaction_read_only=on statement_timeout=5000 connect_timeout=5", nil
}
