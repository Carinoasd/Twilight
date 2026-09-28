package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

// maxDeviceIDLength 是设备 ID 的长度上限。WebUI 不发 X-Twilight-Device，设备 ID
// 实际多为 User-Agent；常见 UA 在 256 字节以内，超长的会被摘要化。
const maxDeviceIDLength = 256

// deviceHeaderPattern 约束客户端自报的 X-Twilight-Device：短、只含安全字符。
// 不合格的值直接忽略、回退到 UA，而不是原样落库——否则任意客户端都能写入超长 /
// 控制字符的设备 ID 撑大 state。
var deviceHeaderPattern = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,128}$`)

// validDeviceID 报告 id 是否为可接受的设备 ID：1..maxDeviceIDLength 字节的可打印 ASCII。
// 用于校验路由参数 :device_id，以及决定 UA 等回退值能否原样作为设备 ID。
func validDeviceID(id string) bool {
	if id == "" || len(id) > maxDeviceIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// normalizeDeviceID 把 UA / IP 等回退值规整成设备 ID：合格的原样保留（与既有记录
// 兼容），超长或含非 ASCII 的取 SHA-256 摘要，保证长度有界且同一来源稳定映射。
func normalizeDeviceID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || validDeviceID(raw) {
		return raw
	}
	sum := sha256.Sum256([]byte(raw))
	return "h:" + hex.EncodeToString(sum[:16])
}

// loginDeviceID 决定一次登录归属的设备：优先合格的 X-Twilight-Device，其次 UA，最后 IP。
func loginDeviceID(header, userAgent, ip string) string {
	if header = strings.TrimSpace(header); header != "" && deviceHeaderPattern.MatchString(header) {
		return header
	}
	return normalizeDeviceID(firstNonEmpty(userAgent, ip))
}

// requireDeviceIDParam 读取并校验路由参数 :device_id；不合格时写 400 并返回 false。
func requireDeviceIDParam(w http.ResponseWriter, params Params) (string, bool) {
	deviceID := params["device_id"]
	if deviceID == "" {
		failWithCode(w, http.StatusBadRequest, ErrDeviceIDRequired, "设备 ID 不能为空")
		return "", false
	}
	if !validDeviceID(deviceID) {
		failWithCode(w, http.StatusBadRequest, ErrDeviceIDInvalid, "设备 ID 格式无效")
		return "", false
	}
	return deviceID, true
}
