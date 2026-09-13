// Package httpapi wires the HTTP surface.
package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/saliheu/go-address-validator/internal/auth"
	"github.com/saliheu/go-address-validator/internal/cache"
	"github.com/saliheu/go-address-validator/internal/model"
	"github.com/saliheu/go-address-validator/internal/validate"
)

type API struct {
	db       *gorm.DB
	validate *validate.Service
	auth     *auth.Service
	cache    *cache.Cache
}

func New(db *gorm.DB, v *validate.Service, a *auth.Service, c *cache.Cache) *API {
	return &API{db: db, validate: v, auth: a, cache: c}
}

func (a *API) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), requestLogger())

	r.GET("/health", a.health)

	auth := r.Group("/auth")
	{
		auth.POST("/register", a.register)
		auth.POST("/login", a.login)
		auth.POST("/refresh", a.refresh)
		auth.POST("/logout", a.logout)
	}

	v1 := r.Group("/v1", a.requireAuth())
	{
		v1.GET("/validate-city", a.validateCity)
		v1.GET("/validate-zipcode", a.validateZipcode)
		v1.GET("/countries", a.listCountries)
		v1.GET("/countries/:code/levels", a.countryLevels)
	}
	return r
}

// ---------- middleware ----------

func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		gin.DefaultWriter.Write([]byte(
			time.Now().Format(time.RFC3339) + " " +
				c.Request.Method + " " + c.Request.URL.Path +
				" " + http.StatusText(c.Writer.Status()) +
				" " + time.Since(start).String() + "\n"))
	}
}

func (a *API) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || strings.TrimSpace(token) == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"error": "missing bearer token"})
			return
		}
		claims, err := a.auth.ParseAccessToken(strings.TrimSpace(token))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"error": "invalid or expired token"})
			return
		}
		c.Set("user_id", claims.UserID)
		c.Next()
	}
}

// ---------- health ----------

func (a *API) health(c *gin.Context) {
	status := gin.H{"status": "ok"}
	code := http.StatusOK

	sqlDB, err := a.db.DB()
	if err != nil || sqlDB.PingContext(c) != nil {
		status["database"] = "down"
		status["status"] = "degraded"
		code = http.StatusServiceUnavailable
	} else {
		status["database"] = "up"
	}

	// Redis being down is explicitly not fatal: validation still works, just
	// slower. Report it without failing the check.
	if a.cache.Ping(c) != nil {
		status["cache"] = "down (serving uncached)"
	} else {
		status["cache"] = "up"
	}
	c.JSON(code, status)
}

// ---------- auth handlers ----------

type credentials struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

func (a *API) register(c *gin.Context) {
	var body credentials
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u, err := a.auth.Register(body.Email, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrEmailTaken) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": u.ID, "email": u.Email})
}

func (a *API) login(c *gin.Context) {
	var body credentials
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pair, err := a.auth.Login(body.Email, body.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	c.JSON(http.StatusOK, pair)
}

type refreshBody struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

func (a *API) refresh(c *gin.Context) {
	var body refreshBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pair, err := a.auth.Refresh(body.RefreshToken)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrTokenReused):
			// 401 with an explicit reason: the client must send the user back
			// through login, not retry.
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":  "refresh token reuse detected",
				"detail": "all sessions for this login were revoked; sign in again",
			})
		case errors.Is(err, auth.ErrInvalidToken):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired refresh token"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not refresh"})
		}
		return
	}
	c.JSON(http.StatusOK, pair)
}

func (a *API) logout(c *gin.Context) {
	var body refreshBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := a.auth.Logout(body.RefreshToken); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not log out"})
		return
	}
	c.Status(http.StatusNoContent)
}

// ---------- validation handlers ----------

func (a *API) validateCity(c *gin.Context) {
	req := validate.CityRequest{
		Country: c.Query("country"),
		City:    c.Query("city"),
		Region:  c.Query("region"),
	}
	if req.Country == "" || req.City == "" {
		c.JSON(http.StatusBadRequest,
			gin.H{"error": "country and city are required"})
		return
	}

	key := cache.Key("city", strings.ToUpper(req.Country), req.City, req.Region)
	var cached validate.CityResponse
	if a.cache.Get(c, key, &cached) {
		c.Header("X-Cache", "HIT")
		c.JSON(http.StatusOK, cached)
		return
	}
	c.Header("X-Cache", "MISS")

	resp, err := a.validate.ValidateCity(c, req)
	if err != nil {
		if errors.Is(err, validate.ErrUnknownCountry) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "validation failed"})
		return
	}
	// Only successful lookups are cached - see the note in package cache.
	if resp.Valid {
		a.cache.Set(c, key, resp)
	}
	c.JSON(http.StatusOK, resp)
}

func (a *API) validateZipcode(c *gin.Context) {
	req := validate.ZipRequest{
		Country: c.Query("country"),
		Code:    c.Query("code"),
		City:    c.Query("city"),
	}
	if req.Country == "" || req.Code == "" {
		c.JSON(http.StatusBadRequest,
			gin.H{"error": "country and code are required"})
		return
	}

	key := cache.Key("zip", strings.ToUpper(req.Country), req.Code, req.City)
	var cached validate.ZipResponse
	if a.cache.Get(c, key, &cached) {
		c.Header("X-Cache", "HIT")
		c.JSON(http.StatusOK, cached)
		return
	}
	c.Header("X-Cache", "MISS")

	resp, err := a.validate.ValidateZipcode(c, req)
	if err != nil {
		if errors.Is(err, validate.ErrUnknownCountry) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "validation failed"})
		return
	}
	if resp.Valid {
		a.cache.Set(c, key, resp)
	}
	c.JSON(http.StatusOK, resp)
}

// ---------- metadata handlers ----------

func (a *API) listCountries(c *gin.Context) {
	var rows []model.Country
	if err := a.db.WithContext(c).Order("code").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not list countries"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"countries": rows, "count": len(rows)})
}

// countryLevels exposes what each admin level is called in a given country.
// This is the API-visible half of the variable-hierarchy design.
func (a *API) countryLevels(c *gin.Context) {
	code := strings.ToUpper(c.Param("code"))
	var rows []model.AdminLevelLabel
	if err := a.db.WithContext(c).
		Where("country_code = ?", code).Order("level").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read levels"})
		return
	}
	if len(rows) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "country not found or has no levels defined"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"country": code, "levels": rows})
}
