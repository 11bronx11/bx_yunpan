package drive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
)

type previewVariantStub struct {
	url       string
	expiresAt time.Time
	found     bool
	err       error
}

func (s previewVariantStub) PreviewURL(context.Context, uuid.UUID) (string, time.Time, bool, error) {
	return s.url, s.expiresAt, s.found, s.err
}

func TestImagePreviewRequiresThumbnail(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Minute)
	lookupErr := errors.New("thumbnail lookup failed")
	for _, test := range []struct {
		name     string
		mimeType string
		variant  PreviewVariant
		wantURL  string
		wantErr  error
	}{
		{name: "missing jpeg thumbnail", mimeType: "image/jpeg", variant: previewVariantStub{}},
		{name: "missing png thumbnail", mimeType: "image/png", variant: previewVariantStub{}},
		{name: "missing gif thumbnail", mimeType: "image/gif", variant: previewVariantStub{}},
		{name: "missing webp thumbnail", mimeType: "image/webp", variant: previewVariantStub{}},
		{name: "no thumbnail provider", mimeType: "image/jpeg"},
		{name: "empty thumbnail URL", mimeType: "image/jpeg", variant: previewVariantStub{found: true}},
		{name: "ready thumbnail", mimeType: "image/jpeg", variant: previewVariantStub{url: "https://example.test/thumbnail.jpg", expiresAt: expiresAt, found: true}, wantURL: "https://example.test/thumbnail.jpg"},
		{name: "thumbnail lookup error", mimeType: "image/jpeg", variant: previewVariantStub{err: lookupErr}, wantErr: lookupErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMoveFixture(t)
			if err := fixture.db.Model(&objectstore.Object{}).Where("id = ?", fixture.object).
				Updates(map[string]any{"mime_type": test.mimeType, "size_bytes": 123076147}).Error; err != nil {
				t.Fatal(err)
			}
			// Signing uses static test credentials and makes no storage requests.
			presigner, err := storageurl.New("storage.example.test", "test-key", "test-secret", "us-east-1", false, "")
			if err != nil {
				t.Fatal(err)
			}
			manager := NewFileManager(fixture.service, objectstore.NewService(fixture.db), nil, presigner, test.variant, time.Minute)
			kind, value, expiry, err := manager.Preview(context.Background(), fixture.ownerID, fixture.file.ID)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("preview error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				if value != "" {
					t.Fatal("failed thumbnail lookup exposed a URL")
				}
				return
			}
			if test.wantURL != "" {
				if kind != "image" || value != test.wantURL || !expiry.Equal(expiresAt) {
					t.Fatalf("ready thumbnail = (%q, %q, %v)", kind, value, expiry)
				}
			} else if kind != "unsupported" || value != "" || !expiry.IsZero() {
				t.Fatalf("missing thumbnail exposed preview = (%q, %q, %v)", kind, value, expiry)
			}
		})
	}
}

func TestUnavailableImagePreviewOmitsURLAndPreservesDownload(t *testing.T) {
	fixture := newMoveFixture(t)
	if err := fixture.db.Model(&objectstore.Object{}).Where("id = ?", fixture.object).
		Updates(map[string]any{"mime_type": "image/jpeg", "size_bytes": 123076147}).Error; err != nil {
		t.Fatal(err)
	}
	presigner, err := storageurl.New("storage.example.test", "test-key", "test-secret", "us-east-1", false, "")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewFileManager(fixture.service, objectstore.NewService(fixture.db), nil, presigner, previewVariantStub{}, time.Minute)
	router := gin.New()
	NewFileHTTP(manager, func(c *gin.Context) { httpapi.SetPrincipalID(c, fixture.ownerID) }).RegisterRoutes(router.Group("/api/v1"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fixture.file.ID.String()+"/preview", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["kind"] != "unsupported" {
		t.Fatalf("unavailable preview response = %s", response.Body.String())
	}
	value, expiresAt, err := manager.DownloadURL(context.Background(), fixture.ownerID, fixture.file.ID)
	if err != nil {
		t.Fatal(err)
	}
	download, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if download.Path != "/test/objects/"+fixtureObjectHash(t, fixture) || download.Query().Get("response-content-disposition") != "attachment; filename=report.pdf" || expiresAt.IsZero() {
		t.Fatal("original download is unavailable or is not an attachment")
	}
}

func fixtureObjectHash(t *testing.T, fixture moveFixture) string {
	t.Helper()
	var object objectstore.Object
	if err := fixture.db.First(&object, "id = ?", fixture.object).Error; err != nil {
		t.Fatal(err)
	}
	return object.SHA256
}
