package api

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestRateLimiterEvictsOldestBucketsAtCapacity 防回归：没有 Redis 时内存桶满了
// 不能拒绝新 key（否则攻击者灌满 1 万个 IP 桶就能让全站新访客 429），而是淘汰
// 最旧的桶；较新的桶继续计数。
func TestRateLimiterEvictsOldestBucketsAtCapacity(t *testing.T) {
	limiter := newRateLimiter(nil)
	base := time.Now().Add(time.Minute)
	for i := 0; i < rateLimiterMaxBuckets; i++ {
		// key-i 的 ResetAt 随 i 递增：key-0 最旧，key-(max-1) 最新。
		limiter.items[fmt.Sprintf("key-%d", i)] = rateBucket{Count: 1, ResetAt: base.Add(time.Duration(i) * time.Millisecond)}
	}

	if !limiter.Allow(context.Background(), "new-key", 10, time.Minute) {
		t.Fatal("new key must be admitted at capacity (oldest buckets evicted instead)")
	}
	if len(limiter.items) > rateLimiterMaxBuckets {
		t.Fatalf("bucket count = %d, want <= %d", len(limiter.items), rateLimiterMaxBuckets)
	}
	if _, ok := limiter.items["key-0"]; ok {
		t.Fatal("oldest bucket should have been evicted")
	}
	newest := fmt.Sprintf("key-%d", rateLimiterMaxBuckets-1)
	if b, ok := limiter.items[newest]; !ok || b.Count != 1 {
		t.Fatalf("newest bucket must be kept with its count: ok=%v %+v", ok, b)
	}
	// 大量新 key 持续涌入也始终放行、桶数有界。
	for i := 0; i < rateLimiterMaxBuckets; i++ {
		if !limiter.Allow(context.Background(), fmt.Sprintf("flood-%d", i), 10, time.Minute) {
			t.Fatalf("flood key %d rejected", i)
		}
	}
	if len(limiter.items) > rateLimiterMaxBuckets {
		t.Fatalf("bucket count = %d, want <= %d", len(limiter.items), rateLimiterMaxBuckets)
	}
}

func TestRateLimiterReclaimsExpiredBucketAtCapacity(t *testing.T) {
	limiter := newRateLimiter(nil)
	future := time.Now().Add(time.Minute)
	for i := 0; i < rateLimiterMaxBuckets; i++ {
		limiter.items[fmt.Sprintf("key-%d", i)] = rateBucket{Count: 1, ResetAt: future}
	}
	limiter.items["key-0"] = rateBucket{Count: 1, ResetAt: time.Now().Add(-time.Second)}

	if !limiter.Allow(context.Background(), "new-key", 10, time.Minute) {
		t.Fatal("new key should use capacity reclaimed from an expired bucket")
	}
	if _, ok := limiter.items["key-0"]; ok {
		t.Fatal("expired bucket was not reclaimed")
	}
	if len(limiter.items) != rateLimiterMaxBuckets {
		t.Fatalf("bucket count = %d, want %d", len(limiter.items), rateLimiterMaxBuckets)
	}
}

func TestRateLimiterAppliesMinimumWindowInMemory(t *testing.T) {
	limiter := newRateLimiter(nil)
	if !limiter.Allow(context.Background(), "short", 1, 0) {
		t.Fatal("first request should be allowed")
	}
	if limiter.Allow(context.Background(), "short", 1, 0) {
		t.Fatal("second request should be limited within the one-second minimum window")
	}
}

// TestRateKeyMatchesLegacyFormat 回归校验：rateKey 与旧 fmt.Sprint(parts...)
// 的字节输出必须逐字节一致，确保限流 key 在跨进程（Redis / 内存桶）语义不变。
func TestRateKeyMatchesLegacyFormat(t *testing.T) {
	cases := [][]any{
		{},
		{"global:", "127.0.0.1"},
		{"login:", "192.168.1.1"},
		{"login:user:", "alice"},
		{"use-code:uid:", int64(42)},
		{"use-code:uid:", 42},
		{"ticket:uid:", int64(123456789)},
		{"emby-probe:", int64(-7)},
		{"apikey:", "hash", "suffix"},
	}
	for _, parts := range cases {
		got := rateKey(parts...)
		want := fmt.Sprint(parts...)
		if got != want {
			t.Fatalf("rateKey(%v) = %q, want %q", parts, got, want)
		}
	}
}
