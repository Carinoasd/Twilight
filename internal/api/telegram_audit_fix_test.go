package api

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 429 带 retry_after 时必须真的退避：用已取消的 ctx 观察函数是否进入了等待分支。
// 修复前变量遮蔽导致 d 恒为 0，直接返回 true（没有等待）。
func TestTelegramRateLimitPauseHonorsRetryAfter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fmt.Errorf("telegram sendMessage failed: Too Many Requests [%s3]", telegramRetryAfterSentinel)
	start := time.Now()
	if telegramRateLimitPauseContext(ctx, err) {
		t.Fatal("retry_after error did not enter backoff wait")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancelled ctx should interrupt backoff promptly")
	}
}
