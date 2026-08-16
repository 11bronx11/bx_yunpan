package upload

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm/schema"
)

func TestPartETagColumn(t *testing.T) {
	parsed, err := schema.Parse(&Part{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse Part schema: %v", err)
	}
	field := parsed.LookUpField("ETag")
	if field == nil {
		t.Fatal("ETag field not found")
	}
	if field.DBName != "etag" {
		t.Fatalf("ETag column = %q, want etag", field.DBName)
	}
}

func TestSessionResponseIncludesConfirmedParts(t *testing.T) {
	session := Session{
		ID: uuid.New(), FolderID: uuid.New(), ConfirmedParts: []Part{{PartNumber: 2, SizeBytes: 42, CompletedAt: time.Unix(1, 0).UTC()}},
	}
	response := sessionResponse(session)
	parts, ok := response["confirmed_parts"].([]gin.H)
	if !ok || len(parts) != 1 {
		t.Fatalf("confirmed_parts = %#v", response["confirmed_parts"])
	}
	if parts[0]["part_number"] != 2 || parts[0]["size_bytes"] != int64(42) {
		t.Fatalf("unexpected confirmed part: %#v", parts[0])
	}
}

func TestWriteErrorDistinguishesExistingFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)

	new(HTTP).writeError(context, ErrFileExists)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "upload.file_exists" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestWriteErrorDistinguishesNameConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)

	new(HTTP).writeError(context, ErrNameConflict)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "upload.name_conflict" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}
