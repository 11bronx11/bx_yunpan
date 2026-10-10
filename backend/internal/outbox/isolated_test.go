package outbox

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var isolatedTestDatabases sync.Map

func isolatedOutboxDB(t *testing.T) *gorm.DB {
	t.Helper()
	if existing, ok := isolatedTestDatabases.Load(t); ok {
		return existing.(*gorm.DB)
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN required")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	schema := "outbox_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"outbox_events", "outbox_deliveries", "outbox_recoveries", "task_results", "task_failures", "task_executions", "legacy_task_imports"} {
		if err := admin.Exec("CREATE TABLE " + schema + "." + table + " (LIKE public." + table + " INCLUDING ALL)").Error; err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasPrefix(dsn, "postgres") {
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	isolatedTestDatabases.Store(t, db)
	t.Cleanup(func() {
		isolatedTestDatabases.Delete(t)
		sql, _ := db.DB()
		_ = sql.Close()
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		root, _ := admin.DB()
		_ = root.Close()
	})
	return db
}

func isolatedRabbitURL(t *testing.T) string {
	t.Helper()
	raw, management := os.Getenv("TEST_RABBITMQ_URL"), os.Getenv("TEST_RABBITMQ_MANAGEMENT_URL")
	if raw == "" || management == "" {
		t.Skip("TEST_RABBITMQ_URL and TEST_RABBITMQ_MANAGEMENT_URL required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	name := "outbox-test-" + uuid.NewString()
	request := func(method, path, body string) {
		req, err := http.NewRequest(method, management+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(u.User.Username(), password)
		req.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode >= 300 {
			t.Fatalf("vhost setup status=%d", response.StatusCode)
		}
	}
	request("PUT", "/api/vhosts/"+name, `{}`)
	request("PUT", "/api/permissions/"+name+"/"+url.PathEscape(u.User.Username()), `{"configure":".*","write":".*","read":".*"}`)
	t.Cleanup(func() { request("DELETE", "/api/vhosts/"+name, "") })
	u.Path = "/" + name
	return u.String()
}
