package api

import (
	"github.com/prejudice-studio/twilight/internal/store"
)

// auditSampleLimit 限制写入审计 detail 的 UID / 码提示列表长度，避免批量操作撑爆
// 8KB detail 上限（超出部分只记总数）。
const auditSampleLimit = 64

// regcodeAuditHint 生成卡码 / 邀请码的审计提示：只保留前后各 4 个字符，中间以
// "…" 代替。审计 detail 会把以 code 结尾的键整体遮成 [REDACTED]，导致无法辨认
// 是哪一张码；改用 code_hint 键记录局部字符，既能对照又不泄露完整可兑换凭据。
// 长度 ≤ 8 的短码只保留首尾各 2 个字符。
func regcodeAuditHint(code string) string {
	runes := []rune(code)
	switch {
	case len(runes) == 0:
		return ""
	case len(runes) <= 4:
		return string(runes[:1]) + "…"
	case len(runes) <= 8:
		return string(runes[:2]) + "…" + string(runes[len(runes)-2:])
	default:
		return string(runes[:4]) + "…" + string(runes[len(runes)-4:])
	}
}

// regcodeAuditHints 对一组码生成提示，最多 auditSampleLimit 条。
func regcodeAuditHints(codes []string) []string {
	out := make([]string, 0, min(len(codes), auditSampleLimit))
	for i, code := range codes {
		if i >= auditSampleLimit {
			break
		}
		out = append(out, regcodeAuditHint(code))
	}
	return out
}

// auditUIDSample 截取最多 auditSampleLimit 个 UID 写入审计。
func auditUIDSample(uids []int64) []int64 {
	if len(uids) > auditSampleLimit {
		return append([]int64(nil), uids[:auditSampleLimit]...)
	}
	return append([]int64{}, uids...)
}

// regcodeAuditFields 是管理员编辑卡码时 before / after 对照的字段集合。
func regcodeAuditFields(rc store.RegCode) map[string]any {
	return map[string]any{
		"type":            rc.Type,
		"days":            rc.Days,
		"validity_time":   rc.ValidityTime,
		"use_count_limit": rc.UseCountLimit,
		"use_count":       rc.UseCount,
		"active":          rc.Active,
		"note":            rc.Note,
	}
}
