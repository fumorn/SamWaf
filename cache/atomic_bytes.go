package cache

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AtomicByteStore is an optional capability, separate from CacheStore so existing
// implementations and mocks need not change. Keys must use a dedicated namespace
// and must not be shared with non-byte cache values. Expiry has millisecond
// precision on both backends and is absolute: writes never extend the deadline.
//
// Reads return (nil, false, nil) for missing/expired keys. TakeBytes consumes a
// value at most once, including when decoding fails. A backend/network error may
// occur after consumption; callers must fail closed, never reconstruct the value.
// All writes and successful reads copy the supplied/returned bytes.
// Use these methods together rather than mixing in legacy GetBytes/Remove.
type AtomicByteStore interface {
	PutBytes(ctx context.Context, key string, value []byte, expiresAt time.Time) error
	PutBytesIfAbsent(ctx context.Context, key string, value []byte, expiresAt time.Time) (bool, error)
	ReadBytes(ctx context.Context, key string) ([]byte, bool, error)
	TakeBytes(ctx context.Context, key string) ([]byte, bool, error)
}

var (
	ErrInvalidExpiry = errors.New("cache expiry must be in the future")
	ErrCacheValue    = errors.New("cache value is not encoded bytes")
)

var (
	_ AtomicByteStore = (*WafCache)(nil)
	_ AtomicByteStore = (*RedisCache)(nil)
)

func byteExpiry(expiresAt time.Time) (time.Time, error) {
	// Normalize before comparing so sub-millisecond deadlines cannot be stored
	// successfully in memory while already expired in Redis.
	expiresAt = time.UnixMilli(expiresAt.UnixMilli())
	if !expiresAt.After(time.Now()) {
		return time.Time{}, ErrInvalidExpiry
	}
	return expiresAt, nil
}

func byteContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrCacheBackend, err)
	}
	return nil
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	out := make([]byte, len(value))
	copy(out, value)
	return out
}

func (c *WafCache) PutBytes(ctx context.Context, key string, value []byte, expiresAt time.Time) error {
	_, err := c.putBytes(ctx, key, value, expiresAt, false)
	return err
}

func (c *WafCache) PutBytesIfAbsent(ctx context.Context, key string, value []byte, expiresAt time.Time) (bool, error) {
	return c.putBytes(ctx, key, value, expiresAt, true)
}

func (c *WafCache) putBytes(ctx context.Context, key string, value []byte, expiresAt time.Time, onlyAbsent bool) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := byteContextError(ctx); err != nil {
		return false, err
	}
	expiry, err := byteExpiry(expiresAt)
	if err != nil {
		return false, err
	}
	now := time.Now()
	if item, exists := c.cache[key]; onlyAbsent && exists && now.Before(item.expireTime) {
		return false, nil
	}
	if err := c.setItemLocked(key, WafCacheItem{
		value: cloneBytes(value), createTime: now, expireTime: expiry, ttl: expiry.Sub(now),
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (c *WafCache) ReadBytes(ctx context.Context, key string) ([]byte, bool, error) {
	return c.readBytes(ctx, key, false)
}

func (c *WafCache) TakeBytes(ctx context.Context, key string) ([]byte, bool, error) {
	return c.readBytes(ctx, key, true)
}

func (c *WafCache) readBytes(ctx context.Context, key string, take bool) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := byteContextError(ctx); err != nil {
		return nil, false, err
	}
	item, found := c.cache[key]
	if !found {
		return nil, false, nil
	}
	if !time.Now().Before(item.expireTime) {
		c.deleteItemLocked(key)
		return nil, false, nil
	}
	if take {
		c.deleteItemLocked(key)
	}
	value, ok := item.value.([]byte)
	if !ok {
		return nil, false, ErrCacheValue
	}
	return cloneBytes(value), true, nil
}
