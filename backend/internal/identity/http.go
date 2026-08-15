package identity

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
	"github.com/gin-gonic/gin"
)

const (
	refreshCookie = "refresh_token"
)

type HTTP struct {
	service *Service
	tokens  *TokenManager
	config  config.Auth
}

func NewHTTP(service *Service, tokens *TokenManager, cfg config.Auth) *HTTP {
	return &HTTP{service: service, tokens: tokens, config: cfg}
}

func (h *HTTP) RegisterRoutes(v1 *gin.RouterGroup) {
	auth := v1.Group("/auth")
	auth.POST("/register", h.register)
	auth.POST("/login", h.login)
	auth.POST("/refresh", h.refresh)
	auth.POST("/logout", h.Authenticate(), h.logout)

	users := v1.Group("/users", h.Authenticate())
	users.GET("/me", h.me)
	users.GET("/me/quota", h.quota)
}

func (h *HTTP) Authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, httpapi.ErrorEnvelope(c, "auth.unauthorized", "authentication required"))
			return
		}
		userID, err := h.tokens.VerifyAccess(parts[1])
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, httpapi.ErrorEnvelope(c, "auth.unauthorized", "authentication required"))
			return
		}
		httpapi.SetPrincipalID(c, userID)
		c.Next()
	}
}

func (h *HTTP) register(c *gin.Context) {
	var request struct {
		Username string `json:"username" binding:"required"`
		Email    string `json:"email"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	session, err := h.service.Register(request.Username, request.Email, request.Password)
	if err != nil {
		h.writeError(c, err)
		return
	}
	h.setRefreshCookie(c, session)
	c.JSON(http.StatusCreated, sessionResponse(session))
}

func (h *HTTP) login(c *gin.Context) {
	var request struct {
		Login    string `json:"login" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	session, err := h.service.Login(request.Login, request.Password)
	if err != nil {
		h.writeError(c, err)
		return
	}
	h.setRefreshCookie(c, session)
	c.JSON(http.StatusOK, sessionResponse(session))
}

func (h *HTTP) refresh(c *gin.Context) {
	rawToken, _ := c.Cookie(refreshCookie)
	session, err := h.service.Rotate(rawToken)
	if err != nil {
		h.clearRefreshCookie(c)
		h.writeError(c, err)
		return
	}
	h.setRefreshCookie(c, session)
	c.JSON(http.StatusOK, sessionResponse(session))
}

func (h *HTTP) logout(c *gin.Context) {
	rawToken, _ := c.Cookie(refreshCookie)
	if err := h.service.Logout(rawToken); err != nil {
		h.writeError(c, err)
		return
	}
	h.clearRefreshCookie(c)
	c.Status(http.StatusNoContent)
}

func (h *HTTP) me(c *gin.Context) {
	userID, _ := httpapi.PrincipalID(c)
	user, err := h.service.User(userID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, userResponse(user))
}

func (h *HTTP) quota(c *gin.Context) {
	userID, _ := httpapi.PrincipalID(c)
	user, err := h.service.User(userID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"quota_bytes":        user.QuotaBytes,
		"used_logical_bytes": user.UsedLogicalBytes,
		"reserved_bytes":     user.ReservedBytes,
	})
}

func (h *HTTP) setRefreshCookie(c *gin.Context, session Session) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(refreshCookie, session.RefreshToken, int(time.Until(session.RefreshExpiry).Seconds()), "/api/v1/auth", h.config.CookieDomain, h.config.CookieSecure, true)
}

func (h *HTTP) clearRefreshCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(refreshCookie, "", -1, "/api/v1/auth", h.config.CookieDomain, h.config.CookieSecure, true)
}

func (h *HTTP) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrConflict):
		c.JSON(http.StatusConflict, httpapi.ErrorEnvelope(c, "identity.conflict", "username or email already exists"))
	case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrDisabled):
		c.JSON(http.StatusUnauthorized, httpapi.ErrorEnvelope(c, "auth.invalid_credentials", "invalid credentials"))
	case errors.Is(err, ErrInvalidInput), errors.Is(err, errWeakPassword):
		c.JSON(http.StatusUnprocessableEntity, httpapi.ErrorEnvelope(c, "identity.invalid_input", err.Error()))
	default:
		c.JSON(http.StatusInternalServerError, httpapi.ErrorEnvelope(c, "internal.error", "internal server error"))
	}
}

func sessionResponse(session Session) gin.H {
	return gin.H{
		"access_token": session.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   int(time.Until(session.AccessExpires).Seconds()),
		"user":         userResponse(session.User),
	}
}

func userResponse(user User) gin.H {
	status := "active"
	if user.Status != UserStatusActive {
		status = "disabled"
	}
	return gin.H{
		"id":         user.ID,
		"username":   user.Username,
		"email":      user.EmailNormalized,
		"status":     status,
		"created_at": user.CreatedAt,
	}
}
