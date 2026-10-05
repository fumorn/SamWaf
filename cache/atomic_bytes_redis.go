package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// SET + PEXPIREAT inside one script supports Redis versions predating SET PXAT.
// Server TIME ensures an elapsed absolute deadline cannot become a live value
// because the application and server clocks differ or the request was delayed.
const putBytesScript = `
local now = redis.call('TIME')
local millis = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
if tonumber(ARGV[2]) <= millis then return -1 end
if ARGV[3] == '1' and redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('PEXPIREAT', KEYS[1], ARGV[2])
return 1
`

// Redis expires keys on GET. Reading and deleting in a single Lua execution is
// atomic across clients/processes, unlike a GET followed by a separate DEL.
const takeBytesScript = `
local value = redis.call('GET', KEYS[1])
if value then redis.call('DEL', KEYS[1]) end
return value
`

func (r *RedisCache) byteOpContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := r.opTimeout
	if timeout <= 0 {
		timeout = defaultRedisOpTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (r *RedisCache) byteBackendError(err error) error {
	return fmt.Errorf("%w: %w", ErrCacheBackend, err)
}

func (r *RedisCache) PutBytes(ctx context.Context, key string, value []byte, expiresAt time.Time) error {
	_, err := r.putBytes(ctx, key, value, expiresAt, false)
	return err
}

func (r *RedisCache) PutBytesIfAbsent(ctx context.Context, key string, value []byte, expiresAt time.Time) (bool, error) {
	return r.putBytes(ctx, key, value, expiresAt, true)
}

func (r *RedisCache) putBytes(ctx context.Context, key string, value []byte, expiresAt time.Time, onlyAbsent bool) (bool, error) {
	if err := byteContextError(ctx); err != nil {
		return false, err
	}
	expiry, err := byteExpiry(expiresAt)
	if err != nil {
		return false, err
	}
	// Match the legacy Redis Set([]byte) encoding without the legacy GetBytes
	// fallback that silently treats malformed JSON as valid bytes.
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrCacheValue, err)
	}
	flag := "0"
	if onlyAbsent {
		flag = "1"
	}
	ctx, cancel := r.byteOpContext(ctx)
	defer cancel()
	result, err := r.client.Eval(ctx, putBytesScript, []string{key}, encoded, expiry.UnixMilli(), flag).Int64()
	if err != nil {
		r.noteErr("PUT_BYTES", key, err)
		return false, r.byteBackendError(err)
	}
	if result == -1 {
		return false, ErrInvalidExpiry
	}
	return result == 1, nil
}

func (r *RedisCache) ReadBytes(ctx context.Context, key string) ([]byte, bool, error) {
	return r.readBytes(ctx, key, false)
}

func (r *RedisCache) TakeBytes(ctx context.Context, key string) ([]byte, bool, error) {
	return r.readBytes(ctx, key, true)
}

func (r *RedisCache) readBytes(ctx context.Context, key string, take bool) ([]byte, bool, error) {
	if err := byteContextError(ctx); err != nil {
		return nil, false, err
	}
	ctx, cancel := r.byteOpContext(ctx)
	defer cancel()
	var raw string
	var err error
	op := "READ_BYTES"
	if take {
		op = "TAKE_BYTES"
		raw, err = r.client.Eval(ctx, takeBytesScript, []string{key}).Text()
	} else {
		raw, err = r.client.Get(ctx, key).Result()
	}
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		r.noteErr(op, key, err)
		return nil, false, r.byteBackendError(err)
	}
	var value []byte
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrCacheValue, err)
	}
	return value, true, nil
}
