package sharing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestShareCommandsRejectInvalidBodies(t *testing.T) {
	h := NewHTTP(nil, nil, func(c *gin.Context) { c.Next() })
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1"))
	for _, path := range []string{"/api/v1/shares", "/api/v1/shares/00000000-0000-0000-0000-000000000001/import"} {
		for _, body := range []string{"{}", "{", `{"file_id":"invalid","target_folder_id":"invalid"}`} {
			t.Run(path+body, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
				}
				var result struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Error.Code != "share.invalid_input" {
					t.Fatalf("code = %s", result.Error.Code)
				}
			})
		}
	}
}
