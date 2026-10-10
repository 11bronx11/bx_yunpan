package identity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/gin-gonic/gin"
)

func TestRefreshCookieOriginProtection(t *testing.T) {
	for _, tc := range []struct {
		name, origin, fetchSite string
		want                    int
	}{
		{"non-browser", "", "", http.StatusNoContent},
		{"same-origin metadata", "http://127.0.0.1:3000", "same-origin", http.StatusNoContent},
		{"same-origin fallback includes port", "http://127.0.0.1:3000", "", http.StatusNoContent},
		{"foreign origin", "https://attacker.invalid", "", http.StatusForbidden},
		{"different port", "http://127.0.0.1:3001", "", http.StatusForbidden},
		{"opaque origin", "null", "", http.StatusForbidden},
		{"malformed origin", "://", "", http.StatusForbidden},
		{"cross-site metadata", "", "cross-site", http.StatusForbidden},
		{"same-site is not same-origin", "http://127.0.0.1:3000", "same-site", http.StatusForbidden},
	} {
		for _, path := range []string{"/refresh", "/logout"} {
			t.Run(tc.name+path, func(t *testing.T) {
				changed := false
				router := gin.New()
				router.POST(path, protectRefreshCookie(), func(c *gin.Context) { changed = true; c.Status(http.StatusNoContent) })
				req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3000"+path, nil)
				req.Header.Set("Origin", tc.origin)
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
				req.AddCookie(&http.Cookie{Name: refreshCookie, Value: "sensitive-session"})
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				if res.Code != tc.want || changed != (tc.want == http.StatusNoContent) {
					t.Fatalf("status = %d, session changed = %v, body = %s", res.Code, changed, res.Body.String())
				}
				if tc.want == http.StatusForbidden && (!strings.Contains(res.Body.String(), "auth.origin_forbidden") || res.Header().Get("Set-Cookie") != "") {
					t.Fatalf("forbidden request changed cookie or returned wrong error: %s", res.Body.String())
				}
			})
		}
	}
}

func TestSessionRoutesRejectOriginBeforeAuthentication(t *testing.T) {
	h := NewHTTP(nil, nil, config.Auth{})
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1"))
	for _, path := range []string{"/api/v1/auth/refresh", "/api/v1/auth/logout"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3000"+path, nil)
		req.Header.Set("Origin", "https://attacker.invalid")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d", path, res.Code)
		}
	}
}
