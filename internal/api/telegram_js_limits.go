package api

import (
	"context"
	"runtime/metrics"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/dop251/goja"
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

// ---- goja 内存护栏 ----
//
// 修复：goja 没有内存上限，管理员预览 'x'.repeat(1<<28) 或循环自拼接字符串就能
// 让 API 进程 OOM（Bot 与 API 同进程时一起挂）。这里做两层有限防护：
//  1. 单次内建调用层：包装 String.prototype.repeat/padStart/padEnd 与
//     Array.prototype.fill，结果超过 developerJSMaxAllocUnits 直接抛 RangeError
//     （这些内建函数在 Go 里一次性分配，Interrupt 拦不住）。
//  2. 进程堆看门狗：脚本运行期间每 10ms 采样一次 Go 堆占用，比脚本开始时增长
//     超过 developerJSHeapGrowthLimit 就 vm.Interrupt 终止脚本。
// 剩余风险：看门狗看的是整个进程的堆，多个脚本或其他请求同时大量分配时可能误杀，
// 也只能在两条 JS 指令之间生效，单条指令内的分配（例如一次把 128MB 字符串再拼
// 一倍）仍会先发生，峰值约为上限的两到三倍；Array 构造、JSON.parse 等其他内建
// 函数没有逐个包装。它是止损而不是隔离，真正的隔离需要把脚本放到独立进程。

const developerJSMaxAllocUnits = 1 << 20 // 单次 repeat/pad/fill 最多 1M 字符或元素

var developerJSHeapGrowthLimit uint64 = 256 << 20

const developerJSAllocationGuardSource = `(function () {
  var MAX = ` + "1048576" + `;
  function define(target, name, fn) {
    Object.defineProperty(target, name, { value: fn, writable: true, configurable: true, enumerable: false });
  }
  var repeat = String.prototype.repeat;
  define(String.prototype, "repeat", function (count) {
    var s = String(this);
    var n = Number(count);
    if (n > 0 && s.length * n > MAX) { throw new RangeError("string too long for sandbox"); }
    return repeat.call(s, count);
  });
  var padStart = String.prototype.padStart;
  define(String.prototype, "padStart", function (len, fill) {
    if (Number(len) > MAX) { throw new RangeError("string too long for sandbox"); }
    return padStart.call(String(this), len, fill);
  });
  var padEnd = String.prototype.padEnd;
  define(String.prototype, "padEnd", function (len, fill) {
    if (Number(len) > MAX) { throw new RangeError("string too long for sandbox"); }
    return padEnd.call(String(this), len, fill);
  });
  var fill = Array.prototype.fill;
  define(Array.prototype, "fill", function (value, start, end) {
    if (Number(this.length) > MAX) { throw new RangeError("array too large for sandbox"); }
    return fill.call(this, value, start, end);
  });
})();`

var developerJSAllocationGuardProgram = goja.MustCompile("developer-js-guard.js", developerJSAllocationGuardSource, true)

// developerJSInstallAllocationGuards 在执行用户脚本前安装内建函数包装。
func developerJSInstallAllocationGuards(vm *goja.Runtime) error {
	_, err := vm.RunProgram(developerJSAllocationGuardProgram)
	return err
}

var developerJSHeapSample = []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
var developerJSHeapSampleMu sync.Mutex

func developerJSHeapBytes() uint64 {
	developerJSHeapSampleMu.Lock()
	defer developerJSHeapSampleMu.Unlock()
	metrics.Read(developerJSHeapSample)
	if developerJSHeapSample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return developerJSHeapSample[0].Value.Uint64()
}

// developerJSStartHeapWatchdog 启动堆增长看门狗，返回停止函数。
func developerJSStartHeapWatchdog(vm *goja.Runtime) func() {
	baseline := developerJSHeapBytes()
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if current := developerJSHeapBytes(); current > baseline && current-baseline > developerJSHeapGrowthLimit {
					vm.Interrupt("memory limit exceeded")
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}
