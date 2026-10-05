package cache

import (
	"container/heap"
	"errors"
	"strings"
	"time"
)

// DefaultCaptchaEntryLimit is a per-WafCache budget shared by challenges,
// consumed-nonce markers, traditional proofs and passes. It limits entries, not
// bytes: callers must still bound payload size. Index overhead is O(limit), with
// one heap node per stored key (no stale nodes after renewal). Other business
// cache namespaces are unaffected. Redis capacity is managed by its deployment;
// a replay-marker store MUST use noeviction rather than evicting live entries.
const DefaultCaptchaEntryLimit = 100000

var ErrCacheCapacity = errors.New("captcha cache entry capacity exhausted")

type captchaExpiry struct {
	key     string
	expires time.Time
	index   int
}

type captchaExpiryHeap []*captchaExpiry

func (h captchaExpiryHeap) Len() int           { return len(h) }
func (h captchaExpiryHeap) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h captchaExpiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *captchaExpiryHeap) Push(v interface{}) {
	n := v.(*captchaExpiry)
	n.index = len(*h)
	*h = append(*h, n)
}
func (h *captchaExpiryHeap) Pop() interface{} {
	old := *h
	n := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	n.index = -1
	return n
}

func isCaptchaByteKey(key string) bool {
	return strings.HasPrefix(key, "CAP_V2_ENGINE:") ||
		strings.HasPrefix(key, "CAP_V2_CLICK:") ||
		strings.HasPrefix(key, "CACHE_CAPTCHA_PASS_v2:")
}

// All helpers below require c.mu. No capacity operation scans the business map.
// Expiration cleanup costs O(k log n) for k expired entries, amortized over their
// insertions; a full cache with no expired entries rejects in O(1).
func (c *WafCache) reclaimCaptchaLocked(now time.Time) {
	for len(c.captchaHeap) > 0 && !c.captchaHeap[0].expires.After(now) {
		c.deleteItemLocked(c.captchaHeap[0].key)
	}
}

func (c *WafCache) deleteItemLocked(key string) {
	delete(c.cache, key)
	if node, ok := c.captchaIndex[key]; ok {
		heap.Remove(&c.captchaHeap, node.index)
		delete(c.captchaIndex, key)
	}
}

func (c *WafCache) setItemLocked(key string, item WafCacheItem) error {
	if isCaptchaByteKey(key) {
		now := time.Now()
		c.reclaimCaptchaLocked(now)
		// Reclamation may take time when many entries expire together. Check the
		// deadline again immediately before admission, not against its start time.
		if !item.expireTime.After(time.Now()) {
			c.deleteItemLocked(key)
			return ErrInvalidExpiry
		}
		if node, exists := c.captchaIndex[key]; exists {
			node.expires = item.expireTime
			heap.Fix(&c.captchaHeap, node.index)
		} else {
			limit := c.captchaLimit
			if limit <= 0 {
				limit = DefaultCaptchaEntryLimit
			}
			if len(c.captchaIndex) >= limit {
				return ErrCacheCapacity
			}
			if c.captchaIndex == nil {
				c.captchaIndex = make(map[string]*captchaExpiry)
			}
			node := &captchaExpiry{key: key, expires: item.expireTime}
			c.captchaIndex[key] = node
			heap.Push(&c.captchaHeap, node)
		}
	}
	c.cache[key] = item
	return nil
}
