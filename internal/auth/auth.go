// Package auth issues and rotates tokens.
//
// Refresh tokens rotate: each refresh invalidates the presented token and
// issues a new one. Rotation alone is not enough though, because it does not
// say what to do when an already-used token shows up again. That can only mean
// the token leaked - either the attacker or the real user is replaying it, and
// there is no way to tell which from the request alone.
//
// The response here is family revocation. Every token descended from one login
// shares a FamilyID; a replay revokes the whole family, forcing a fresh login.
// The legitimate user is logged out once, and the stolen token is worthless.
//
// Tokens are stored as SHA-256 hashes. A database leak then yields no usable
// credentials. (SHA-256, not bcrypt: these are 256-bit random values, not
// human-chosen passwords, so there is nothing to brute-force and the per-request
// cost of a slow KDF is not worth paying.)
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/saliheu/go-address-validator/internal/model"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrTokenReused        = errors.New("refresh token reuse detected; session family revoked")
	ErrEmailTaken         = errors.New("email already registered")
)

type Service struct {
	db            *gorm.DB
	accessSecret  []byte
	refreshSecret []byte
	accessTTL     time.Duration
	refreshTTL    time.Duration
}

func New(db *gorm.DB, accessSecret, refreshSecret string, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{
		db:            db,
		accessSecret:  []byte(accessSecret),
		refreshSecret: []byte(refreshSecret),
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
	}
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"` // access token lifetime, seconds
	TokenType    string `json:"token_type"`
}

type Claims struct {
	UserID uint   `json:"uid"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

// ---------- registration / login ----------

func (s *Service) Register(email, password string) (*model.User, error) {
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	var count int64
	if err := s.db.Model(&model.User{}).Where("email = ?", email).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, ErrEmailTaken
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	u := &model.User{Email: email, PasswordHash: string(hash)}
	if err := s.db.Create(u).Error; err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Service) Login(email, password string) (*TokenPair, error) {
	var u model.User
	if err := s.db.Where("email = ?", email).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Run a hash comparison anyway so a missing account and a wrong
			// password take about the same time. Otherwise response latency
			// tells an attacker which emails are registered.
			bcrypt.CompareHashAndPassword(
				[]byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"),
				[]byte(password))
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return s.issuePair(&u, newFamilyID())
}

// ---------- rotation ----------

func (s *Service) Refresh(refreshToken string) (*TokenPair, error) {
	hash := hashToken(refreshToken)

	var stored model.RefreshToken
	if err := s.db.Where("token_hash = ?", hash).First(&stored).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidToken
		}
		return nil, err
	}

	// Replay: this token was already exchanged. Burn the family.
	if stored.UsedAt != nil {
		if err := s.revokeFamily(stored.FamilyID); err != nil {
			return nil, err
		}
		return nil, ErrTokenReused
	}
	if stored.RevokedAt != nil {
		return nil, ErrInvalidToken
	}
	if time.Now().After(stored.ExpiresAt) {
		return nil, ErrInvalidToken
	}

	var u model.User
	if err := s.db.First(&u, stored.UserID).Error; err != nil {
		return nil, ErrInvalidToken
	}

	// Mark used and issue the successor atomically: a crash between the two
	// must not leave the user holding a token that is already spent.
	var pair *TokenPair
	err := s.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if err := tx.Model(&stored).Update("used_at", &now).Error; err != nil {
			return err
		}
		var innerErr error
		pair, innerErr = s.issuePairTx(tx, &u, stored.FamilyID)
		return innerErr
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Logout revokes the whole family the token belongs to.
func (s *Service) Logout(refreshToken string) error {
	var stored model.RefreshToken
	if err := s.db.Where("token_hash = ?", hashToken(refreshToken)).First(&stored).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // already gone; nothing to do
		}
		return err
	}
	return s.revokeFamily(stored.FamilyID)
}

func (s *Service) revokeFamily(familyID string) error {
	now := time.Now()
	return s.db.Model(&model.RefreshToken{}).
		Where("family_id = ? AND revoked_at IS NULL", familyID).
		Update("revoked_at", &now).Error
}

// ---------- verification ----------

func (s *Service) ParseAccessToken(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		// Pin the algorithm. Without this check a token with alg=none, or one
		// signed with the public key under HMAC, can be forged.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return s.accessSecret, nil
	})
	if err != nil || !tok.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// ---------- issuing ----------

func (s *Service) issuePair(u *model.User, familyID string) (*TokenPair, error) {
	return s.issuePairTx(s.db, u, familyID)
}

func (s *Service) issuePairTx(tx *gorm.DB, u *model.User, familyID string) (*TokenPair, error) {
	now := time.Now()

	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{
		UserID: u.ID,
		Email:  u.Email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprint(u.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
			Issuer:    "go-address-validator",
		},
	}).SignedString(s.accessSecret)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	// The refresh token is opaque random bytes, not a JWT. It carries no
	// claims, so it cannot be read or trusted client-side, and revocation is a
	// database fact rather than something to work around.
	refresh, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	rec := &model.RefreshToken{
		UserID:    u.ID,
		TokenHash: hashToken(refresh),
		FamilyID:  familyID,
		ExpiresAt: now.Add(s.refreshTTL),
	}
	if err := tx.Create(rec).Error; err != nil {
		return nil, fmt.Errorf("persist refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(s.accessTTL.Seconds()),
		TokenType:    "Bearer",
	}, nil
}

// ---------- helpers ----------

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func newFamilyID() string {
	s, err := randomToken(18)
	if err != nil {
		// randomness failure is not recoverable; fall back to a timestamp so
		// the caller still gets a distinct family rather than an empty one
		return fmt.Sprintf("fam-%d", time.Now().UnixNano())
	}
	return s
}
