package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Construct without the production cleanup goroutine; reads must enforce expiry
// themselves, independent of whether periodic cleanup has run.
func TestMemoryAtomicByteStore(t *testing.T) {
	c := &WafCache{cache: make(map[string]WafCacheItem)}
	testAtomicByteStore(t, c, c)
}

func testAtomicByteStore(t *testing.T, store, peer AtomicByteStore) {
	t.Helper()
	ctx := context.Background()
	keyPrefix := fmt.Sprintf("CAP_ATOMIC_TEST_%d_", time.Now().UnixNano())
	key := func(s string) string { return keyPrefix + s }
	deadline := func() time.Time { return time.Now().Add(5 * time.Second) }
	for _, read := range []func(context.Context, string) ([]byte, bool, error){store.ReadBytes, store.TakeBytes} {
		if _, ok, err := read(ctx, key("missing")); ok || err != nil {
			t.Fatalf("missing: found=%v err=%v", ok, err)
		}
	}
	for _, value := range [][]byte{nil, {}, {0, 255, '\n'}, []byte(`"YWJj"`)} {
		input := cloneBytes(value)
		if err := store.PutBytes(ctx, key("roundtrip"), input, deadline()); err != nil {
			t.Fatal(err)
		}
		if len(input) > 0 {
			input[0] ^= 0xff
		}
		got, ok, err := peer.ReadBytes(ctx, key("roundtrip"))
		if err != nil || !ok || !bytes.Equal(got, value) || (got == nil) != (value == nil) {
			t.Fatalf("roundtrip got=%v want=%v found=%v err=%v", got, value, ok, err)
		}
		if len(got) > 0 {
			got[0] ^= 0xff
		}
		got, ok, err = peer.TakeBytes(ctx, key("roundtrip"))
		if err != nil || !ok || !bytes.Equal(got, value) {
			t.Fatalf("copy-out/take got=%v want=%v found=%v err=%v", got, value, ok, err)
		}
		if _, ok, err := store.TakeBytes(ctx, key("roundtrip")); ok || err != nil {
			t.Fatalf("second take found=%v err=%v", ok, err)
		}
	}
	if err := store.PutBytes(ctx, key("invalid"), nil, time.Now().Add(-time.Second)); !errors.Is(err, ErrInvalidExpiry) {
		t.Fatalf("past expiry: %v", err)
	}
	if ok, err := store.PutBytesIfAbsent(ctx, key("invalid"), nil, time.Time{}); ok || !errors.Is(err, ErrInvalidExpiry) {
		t.Fatalf("zero expiry: %v %v", ok, err)
	}
	expires := time.Now().Add(100 * time.Millisecond)
	if err := store.PutBytes(ctx, key("expiry"), []byte("old"), expires); err != nil {
		t.Fatal(err)
	}
	if ok, err := peer.PutBytesIfAbsent(ctx, key("expiry"), []byte("new"), deadline()); ok || err != nil {
		t.Fatalf("NX replaced live value: %v %v", ok, err)
	}
	time.Sleep(time.Until(expires) + 20*time.Millisecond)
	if _, ok, err := store.ReadBytes(ctx, key("expiry")); ok || err != nil {
		t.Fatalf("expired read: %v %v", ok, err)
	}
	if ok, err := peer.PutBytesIfAbsent(ctx, key("expiry"), []byte("new"), deadline()); !ok || err != nil {
		t.Fatalf("NX did not replace expired value: %v %v", ok, err)
	}
	_, _, _ = peer.TakeBytes(ctx, key("expiry"))
	// No preceding read/cleanup may be required for Take or NX to see expiry.
	shortExpiry := time.Now().Add(80 * time.Millisecond)
	for _, suffix := range []string{"expired-take", "expired-nx"} {
		if err := store.PutBytes(ctx, key(suffix), nil, shortExpiry); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Until(shortExpiry) + 20*time.Millisecond)
	if _, ok, err := peer.TakeBytes(ctx, key("expired-take")); ok || err != nil {
		t.Fatalf("expired take: %v %v", ok, err)
	}
	if ok, err := peer.PutBytesIfAbsent(ctx, key("expired-nx"), nil, deadline()); !ok || err != nil {
		t.Fatalf("expired NX without cleanup: %v %v", ok, err)
	}
	_, _, _ = peer.TakeBytes(ctx, key("expired-nx"))
	// Rewrites use the supplied absolute expiry rather than retaining the first
	// write time (the legacy memory SetWithTTl behavior).
	if err := store.PutBytes(ctx, key("rewrite"), nil, time.Now().Add(50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := peer.PutBytes(ctx, key("rewrite"), []byte("new"), deadline()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond)
	if _, ok, err := store.TakeBytes(ctx, key("rewrite")); !ok || err != nil {
		t.Fatalf("rewrite expiry: %v %v", ok, err)
	}
	// Two independent Redis clients (or two handles to the same memory store)
	// contend: exactly one producer and exactly one consumer may succeed.
	var wg sync.WaitGroup
	var puts, takes atomic.Int32
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := store
			if i%2 == 0 {
				s = peer
			}
			ok, err := s.PutBytesIfAbsent(ctx, key("race"), []byte("ticket"), deadline())
			if err != nil {
				t.Errorf("concurrent put: %v", err)
			}
			if ok {
				puts.Add(1)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := store
			if i%2 == 0 {
				s = peer
			}
			_, ok, err := s.TakeBytes(ctx, key("race"))
			if err != nil {
				t.Errorf("concurrent take: %v", err)
			}
			if ok {
				takes.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if puts.Load() != 1 || takes.Load() != 1 {
		t.Fatalf("producers=%d consumers=%d", puts.Load(), takes.Load())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.PutBytes(cancelled, key("cancel"), nil, deadline()); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCacheBackend) {
		t.Fatalf("cancelled write: %v", err)
	}
	if _, _, err := store.TakeBytes(cancelled, key("cancel")); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCacheBackend) {
		t.Fatalf("cancelled take: %v", err)
	}
}

func TestMemoryAtomicBytesExpiredAndWrongType(t *testing.T) {
	c := &WafCache{cache: make(map[string]WafCacheItem)}
	ctx := context.Background()
	now := time.Now()
	c.cache["expired"] = WafCacheItem{value: []byte("x"), expireTime: now}
	if _, ok, err := c.TakeBytes(ctx, "expired"); ok || err != nil {
		t.Fatalf("expired take: %v %v", ok, err)
	}
	c.cache["expired-nx"] = WafCacheItem{value: []byte("x"), expireTime: now}
	if ok, err := c.PutBytesIfAbsent(ctx, "expired-nx", nil, now.Add(time.Minute)); !ok || err != nil {
		t.Fatalf("expired NX: %v %v", ok, err)
	}
	c.SetWithTTl("wrong", 123, time.Minute)
	if _, _, err := c.ReadBytes(ctx, "wrong"); !errors.Is(err, ErrCacheValue) {
		t.Fatalf("wrong read: %v", err)
	}
	if _, _, err := c.TakeBytes(ctx, "wrong"); !errors.Is(err, ErrCacheValue) {
		t.Fatalf("wrong take: %v", err)
	}
	if _, ok, err := c.TakeBytes(ctx, "wrong"); ok || err != nil {
		t.Fatalf("wrong value not consumed: %v %v", ok, err)
	}
}

// Opt-in integration test, no new dependency and no global FLUSHDB. Use a
// disposable Redis endpoint, e.g. SAMWAF_TEST_REDIS_ADDR=127.0.0.1:16379.
func TestRedisAtomicByteStoreIntegration(t *testing.T) {
	addr := os.Getenv("SAMWAF_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set SAMWAF_TEST_REDIS_ADDR for Redis integration tests")
	}
	newStore := func() *RedisCache {
		client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
		t.Cleanup(func() { _ = client.Close() })
		return &RedisCache{client: client, opTimeout: time.Second}
	}
	store, peer := newStore(), newStore()
	ctx := context.Background()
	if err := store.client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	testAtomicByteStore(t, store, peer)
	key := fmt.Sprintf("CAP_ATOMIC_CORRUPT_%d", time.Now().UnixNano())
	if err := store.client.Set(ctx, key, "invalid json", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.client.Del(ctx, key) })
	if _, _, err := store.ReadBytes(ctx, key); !errors.Is(err, ErrCacheValue) {
		t.Fatalf("corrupt read: %v", err)
	}
	if _, _, err := peer.TakeBytes(ctx, key); !errors.Is(err, ErrCacheValue) {
		t.Fatalf("corrupt take: %v", err)
	}
	if _, ok, err := store.ReadBytes(ctx, key); ok || err != nil {
		t.Fatalf("corrupt not consumed: %v %v", ok, err)
	}
	// Closing only this test-owned connection proves backend errors cannot be
	// mistaken for cache misses and does not disrupt any external server.
	_ = peer.client.Close()
	if _, _, err := peer.ReadBytes(ctx, key); !errors.Is(err, ErrCacheBackend) {
		t.Fatalf("backend failure: %v", err)
	}
	if _, _, err := peer.TakeBytes(ctx, key); !errors.Is(err, ErrCacheBackend) {
		t.Fatalf("backend take failure: %v", err)
	}
	if err := peer.PutBytes(ctx, key, nil, time.Now().Add(time.Minute)); !errors.Is(err, ErrCacheBackend) {
		t.Fatalf("backend put failure: %v", err)
	}
}
