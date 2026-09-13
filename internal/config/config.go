// Package config reads settings from the environment.
//
// Secrets are required to be set explicitly outside development: shipping a
// service that silently falls back to a hard-coded JWT secret is how test
// credentials end up signing production tokens.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DB struct {
		Host, Port, User, Password, Name, SSLMode string
	}
	Redis struct {
		Addr     string
		Password string
		TTL      time.Duration
	}
	Auth struct {
		AccessSecret  string
		RefreshSecret string
		AccessTTL     time.Duration
		RefreshTTL    time.Duration
	}
	Port    string
	GinMode string
}

func Load() (*Config, error) {
	c := &Config{}

	c.DB.Host = env("DB_HOST", "localhost")
	c.DB.Port = env("DB_PORT", "5432")
	c.DB.User = env("DB_USER", "postgres")
	c.DB.Password = env("DB_PASSWORD", "postgres")
	c.DB.Name = env("DB_NAME", "addressvalidator")
	c.DB.SSLMode = env("DB_SSLMODE", "disable")

	c.Redis.Addr = env("REDIS_ADDR", "localhost:6379")
	c.Redis.Password = env("REDIS_PASSWORD", "")
	c.Redis.TTL = time.Duration(envInt("CACHE_TTL_SECONDS", 3600)) * time.Second

	c.Auth.AccessSecret = os.Getenv("JWT_ACCESS_SECRET")
	c.Auth.RefreshSecret = os.Getenv("JWT_REFRESH_SECRET")
	c.Auth.AccessTTL = time.Duration(envInt("ACCESS_TOKEN_TTL_MINUTES", 15)) * time.Minute
	c.Auth.RefreshTTL = time.Duration(envInt("REFRESH_TOKEN_TTL_HOURS", 168)) * time.Hour

	c.Port = env("PORT", "8080")
	c.GinMode = env("GIN_MODE", "debug")

	if c.Auth.AccessSecret == "" || c.Auth.RefreshSecret == "" {
		return nil, fmt.Errorf("JWT_ACCESS_SECRET and JWT_REFRESH_SECRET must be set (see .env.example)")
	}
	if c.Auth.AccessSecret == c.Auth.RefreshSecret {
		return nil, fmt.Errorf("access and refresh secrets must differ, otherwise a refresh token is accepted as an access token")
	}
	return c, nil
}

func (c *Config) Debug() bool { return c.GinMode != "release" }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
