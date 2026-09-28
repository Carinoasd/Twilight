package api

import (
	"context"
	"time"

	"github.com/dlclark/regexp2/v2"
	"go.uber.org/zap"
)

// developerJSRegexpMatchTimeout 是单次正则匹配（regexp2 回溯引擎）的时间上限。
//
// 修复：goja 的 vm.Interrupt 只在 JS 指令之间检查，regexp2 做灾难性回溯时
// 不会检查中断，8 秒执行超时和 ctx 取消都拦不住（实测 1 分钟以上）。goja 只在
// 模式需要回溯特性（反向引用、环视等）时才用 regexp2，其余走线性时间的 RE2。
// regexp2 支持 MatchTimeout，但只在 Compile 时读取包级 DefaultMatchTimeout，
// 所以在包初始化时设置；超时后 goja 把匹配视为「未命中」（返回 null/false），
// 不会抛异常。整个进程里 regexp2 只被 goja 间接使用，不影响其他代码。
const developerJSRegexpMatchTimeout = 250 * time.Millisecond

func init() {
	regexp2.DefaultMatchTimeout = developerJSRegexpMatchTimeout
}

// telegramUpdateHandleLimit 是单条 Telegram update 占用批处理的最长时间。
// 修复：批处理要等整批处理完才推进 offset，一条卡住的 update（慢脚本、慢上游）
// 会拖住所有聊天室。超过上限后放行：该 update 在后台继续跑完（ctx 已取消，
// JS 会被中断），批处理不再等待。变量形式便于测试缩短。
var telegramUpdateHandleLimit = 30 * time.Second

// telegramBoundedUpdateHandler 给单条 update 的处理加上时间上限。
func telegramBoundedUpdateHandler(limit time.Duration, handle func(context.Context, *telegramUpdate)) func(context.Context, *telegramUpdate) {
	if limit <= 0 {
		return handle
	}
	return func(ctx context.Context, update *telegramUpdate) {
		updateCtx, cancel := context.WithTimeout(ctx, limit)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer cancel()
			handle(updateCtx, update)
		}()
		select {
		case <-done:
		case <-updateCtx.Done():
			var updateID int64
			if update != nil {
				updateID = update.UpdateID
			}
			zap.L().Warn("telegram update exceeded handling limit; continuing in background", zap.Int64("update_id", updateID), zap.Duration("limit", limit))
		}
	}
}
