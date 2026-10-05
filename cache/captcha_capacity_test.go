package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newCapacityCache(limit int) *WafCache {
	return &WafCache{cache: make(map[string]WafCacheItem), captchaLimit: limit}
}

func TestCaptchaCapacitySharedAndFailClosed(t *testing.T) {
	c := newCapacityCache(3)
	ctx := context.Background()
	expiry := time.Now().Add(time.Minute)
	keys := []string{"CAP_V2_ENGINE:nonce", "CAP_V2_CLICK:proof", "CACHE_CAPTCHA_PASS_v2:pass"}
	for _, key := range keys {
		if err := c.PutBytes(ctx, key, []byte(key), expiry); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.PutBytes(ctx, "CAP_V2_ENGINE:excess", nil, expiry); !errors.Is(err, ErrCacheCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	if ok, err := c.PutBytesIfAbsent(ctx, "CAP_V2_CLICK:excess", nil, expiry); ok || !errors.Is(err, ErrCacheCapacity) {
		t.Fatalf("NX capacity: %v %v", ok, err)
	}
	for _, key := range keys {
		if _, ok, err := c.ReadBytes(ctx, key); !ok || err != nil {
			t.Fatalf("live credential evicted: %v %v", ok, err)
		}
	}
	if err := c.PutBytes(ctx, keys[0], []byte("overwrite"), expiry); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.PutBytesIfAbsent(ctx, keys[0], nil, expiry); ok || err != nil {
		t.Fatalf("existing NX must not be capacity error: %v %v", ok, err)
	}
	for i := 0; i < 10; i++ {
		if err := c.PutBytes(ctx, fmt.Sprintf("BUSINESS:%d", i), nil, expiry); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.captchaIndex) != 3 || len(c.captchaHeap) != 3 {
		t.Fatal("accounting grew on overwrites/business entries")
	}
	c.Remove(keys[0])
	if err := c.PutBytes(ctx, "CAP_V2_ENGINE:replacement", nil, expiry); err != nil {
		t.Fatalf("legacy remove did not release slot: %v", err)
	}
}

func TestCaptchaCapacityExpiryReclaimsWithoutScan(t *testing.T) {
	c := newCapacityCache(2)
	ctx := context.Background()
	short := time.Now().Add(60 * time.Millisecond)
	long := time.Now().Add(time.Minute)
	if err := c.PutBytes(ctx, "CAP_V2_ENGINE:expired", nil, short); err != nil {
		t.Fatal(err)
	}
	if err := c.PutBytes(ctx, "CAP_V2_ENGINE:live", nil, long); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(short) + 10*time.Millisecond)
	if err := c.PutBytes(ctx, "CAP_V2_CLICK:new", nil, long); err != nil {
		t.Fatalf("expired slot was not reclaimed: %v", err)
	}
	if _, ok := c.cache["CAP_V2_ENGINE:expired"]; ok {
		t.Fatal("expired value retained")
	}
	if _, ok, _ := c.ReadBytes(ctx, "CAP_V2_ENGINE:live"); !ok {
		t.Fatal("live nonce evicted")
	}
	if len(c.captchaHeap) != 2 || len(c.captchaIndex) != 2 {
		t.Fatal("incorrect accounting")
	}
}

func TestCaptchaCapacityLegacyMutationAccounting(t *testing.T) {
	ctx := context.Background()
	removers := map[string]func(*WafCache, string){
		"remove":     func(c *WafCache, k string) { c.Remove(k) },
		"take":       func(c *WafCache, k string) { _, _, _ = c.TakeBytes(ctx, k) },
		"get":        func(c *WafCache, k string) { c.Get(k) },
		"exists":     func(c *WafCache, k string) { c.IsKeyExist(k) },
		"expiry":     func(c *WafCache, k string) { _, _ = c.GetExpireTime(k) },
		"read":       func(c *WafCache, k string) { _, _, _ = c.ReadBytes(ctx, k) },
		"cleanup":    func(c *WafCache, k string) { c.ClearExpirationCache() },
		"list":       func(c *WafCache, k string) { c.ListAvailableKeys() },
		"listprefix": func(c *WafCache, k string) { c.ListAvailableKeysWithPrefix("CAP_V2_ENGINE:") },
		"touch":      func(c *WafCache, k string) { _ = c.Touch(k, time.Second) },
		"getasex":    func(c *WafCache, k string) { var out []byte; _ = c.GetAsEx(k, &out, time.Second) },
	}
	for name, remove := range removers {
		t.Run(name, func(t *testing.T) {
			c := newCapacityCache(1)
			key := "CAP_V2_ENGINE:test"
			c.SetWithTTl(key, []byte("value"), 20*time.Millisecond)
			if len(c.captchaIndex) != 1 {
				t.Fatal("legacy write not indexed")
			}
			time.Sleep(30 * time.Millisecond)
			remove(c, key)
			if len(c.captchaHeap) != 0 || len(c.captchaIndex) != 0 {
				t.Fatal("legacy deletion left stale index")
			}
		})
	}
	c := newCapacityCache(1)
	key := "CAP_V2_ENGINE:renew"
	c.SetWithTTl(key, []byte("value"), time.Minute)
	for i := 0; i < 200; i++ {
		c.SetWithTTlRenewTime(key, []byte("value"), time.Minute)
		if err := c.Touch(key, time.Minute); err != nil {
			t.Fatal(err)
		}
		var out []byte
		if err := c.GetAsEx(key, &out, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.captchaHeap) != 1 || len(c.captchaIndex) != 1 {
		t.Fatal("renewals accumulated stale heap nodes")
	}
	if !c.captchaHeap[0].expires.Equal(c.cache[key].expireTime) {
		t.Fatal("renewed deadline not indexed")
	}
	// Legacy writes cannot bypass the quota even though their API has no error.
	c.SetWithTTl("CAP_V2_ENGINE:bypass", nil, time.Minute)
	if c.IsKeyExist("CAP_V2_ENGINE:bypass") {
		t.Fatal("legacy write bypassed quota")
	}
}

func TestCaptchaCapacityConcurrentAdmissions(t *testing.T) {
	c := newCapacityCache(10)
	var wg sync.WaitGroup
	var admitted atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := c.PutBytes(context.Background(), fmt.Sprintf("CAP_V2_ENGINE:%d", i), nil, time.Now().Add(time.Minute))
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrCacheCapacity) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 10 || len(c.captchaHeap) != 10 || len(c.captchaIndex) != 10 {
		t.Fatalf("admitted=%d heap=%d index=%d", admitted.Load(), len(c.captchaHeap), len(c.captchaIndex))
	}
}

func TestCaptchaCapacityDefaultBudget(t *testing.T) {
	c := newCapacityCache(0)
	ctx := context.Background()
	expiry := time.Now().Add(time.Minute)
	for i := 0; i < DefaultCaptchaEntryLimit; i++ {
		if err := c.PutBytes(ctx, fmt.Sprintf("CAP_V2_ENGINE:%d", i), nil, expiry); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.PutBytes(ctx, "CAP_V2_ENGINE:full", nil, expiry); !errors.Is(err, ErrCacheCapacity) {
		t.Fatalf("default capacity: %v", err)
	}
	if len(c.captchaHeap) != DefaultCaptchaEntryLimit {
		t.Fatal("unexpected heap budget")
	}
}

// Run with -run '^$' -bench BenchmarkCaptchaCapacityFull -benchmem. Comparing
// small and production-sized full caches catches accidental O(n) admission scans.
func BenchmarkCaptchaCapacityFull(b *testing.B) {
	for _, limit := range []int{100, DefaultCaptchaEntryLimit} {
		b.Run(fmt.Sprint(limit), func(b *testing.B) {
			c := newCapacityCache(limit)
			ctx := context.Background()
			expiry := time.Now().Add(time.Hour)
			for i := 0; i < limit; i++ {
				if err := c.PutBytes(ctx, fmt.Sprintf("CAP_V2_ENGINE:%d", i), nil, expiry); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := c.PutBytes(ctx, "CAP_V2_ENGINE:rejected", nil, expiry); !errors.Is(err, ErrCacheCapacity) {
					b.Fatal(err)
				}
			}
		})
	}
}
