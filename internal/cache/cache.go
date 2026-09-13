// Package cache is a thin cache-aside layer over Redis.
//
// Two deliberate choices:
//
//   - Cache misses and Redis outages are never fatal. If Redis is down the
//     service gets slower, not broken. A cache that can take the API down with
//     it is worse than no cache.
//   - Only successful validations are cached. Negative results are cheap to
//     recompute and caching them lets one bad import poison answers for a full
//     TTL after the data is fixed.
package cache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

func New(addr, password string, ttl time.Duration) *Cache {
	return &Cache{
		rdb: redis.NewClient(&redis.Options{
			Addr:         addr,
			Password:     password,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  time.Second,
			WriteTimeout: time.Second,
		}),
		ttl: ttl,
	}
}

func (c *Cache) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *Cache) Close() error { return c.rdb.Close() }

// Key builds a stable cache key. Parts are joined and hashed so that a long
// free-text place name cannot produce an unbounded key, and so that a value
// containing the separator cannot collide with a different set of parts.
func Key(namespace string, parts ...string) string {
	joined := strings.Join(parts, "\x1f") // unit separator: cannot appear in normalized input
	sum := sha1.Sum([]byte(joined))
	return "av:" + namespace + ":" + hex.EncodeToString(sum[:])
}

// Get unmarshals a cached value into dst. It reports false on a miss, on a
// decode failure, or when Redis is unreachable.
func (c *Cache) Get(ctx context.Context, key string, dst any) bool {
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			log.Printf("cache get %s: %v (continuing without cache)", key, err)
		}
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		// A stale value from an older response shape: drop it rather than
		// failing the request.
		log.Printf("cache decode %s: %v (evicting)", key, err)
		c.rdb.Del(ctx, key)
		return false
	}
	return true
}

// Set stores a value. Failures are logged and swallowed.
func (c *Cache) Set(ctx context.Context, key string, val any) {
	raw, err := json.Marshal(val)
	if err != nil {
		log.Printf("cache encode %s: %v", key, err)
		return
	}
	if err := c.rdb.Set(ctx, key, raw, c.ttl).Err(); err != nil {
		log.Printf("cache set %s: %v (continuing)", key, err)
	}
}
