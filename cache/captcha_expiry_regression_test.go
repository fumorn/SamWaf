package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Exercise the final admission check directly with a fixed elapsed deadline:
// no scheduler timing, sleeps, or randomly short TTLs are needed to reproduce
// the state reached when a deadline passes after PutBytes's initial validation.
func TestCaptchaAdmissionExpiredDeadlineReturnsError(t *testing.T) {
	for _, existing := range []bool{false, true} {
		c := newCapacityCache(1)
		key := "CAP_V2_ENGINE:deadline"
		if existing {
			if err := c.PutBytes(context.Background(), key, []byte("old"), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		c.mu.Lock()
		err := c.setItemLocked(key, WafCacheItem{
			value:      []byte("already expired"),
			createTime: time.Unix(1, 0), expireTime: time.Unix(2, 0), ttl: time.Second,
		})
		_, present := c.cache[key]
		indexLen, heapLen := len(c.captchaIndex), len(c.captchaHeap)
		c.mu.Unlock()
		if !errors.Is(err, ErrInvalidExpiry) {
			t.Fatalf("existing=%v: must not report successful write, err=%v", existing, err)
		}
		if present || indexLen != 0 || heapLen != 0 {
			t.Fatalf("existing=%v: expired write/index retained", existing)
		}
	}
}

// Inject a full index plus one unindexed legacy item to force a deterministic
// setItemLocked error during renewal. Such a fixture is intentionally not a
// normal production state: it tests error propagation independently of clocks.
func TestCaptchaRenewalPropagatesAdmissionError(t *testing.T) {
	for _, method := range []string{"touch", "getasex"} {
		t.Run(method, func(t *testing.T) {
			c := newCapacityCache(1)
			expiry := time.Now().Add(time.Hour)
			if err := c.PutBytes(context.Background(), "CAP_V2_ENGINE:occupied", nil, expiry); err != nil {
				t.Fatal(err)
			}
			key := "CAP_V2_ENGINE:legacy-unindexed"
			now := time.Now()
			c.cache[key] = WafCacheItem{value: []byte("old"), createTime: now, expireTime: now.Add(time.Hour), ttl: time.Hour}
			var err error
			if method == "touch" {
				err = c.Touch(key, time.Minute)
			} else {
				out := []byte("unchanged")
				err = c.GetAsEx(key, &out, time.Minute)
				if string(out) != "unchanged" {
					t.Fatal("failed renewal returned a value as if successful")
				}
			}
			if !errors.Is(err, ErrCacheCapacity) {
				t.Fatalf("admission error swallowed: %v", err)
			}
			// Also verifies GetAsEx released mu on its early error return.
			c.Remove(key)
			if _, ok, err := c.ReadBytes(context.Background(), "CAP_V2_ENGINE:occupied"); !ok || err != nil {
				t.Fatalf("live nonce affected: %v %v", ok, err)
			}
		})
	}
}
