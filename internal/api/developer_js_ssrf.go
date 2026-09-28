package api

import (
	"net/netip"
)

// developerJSBlockedPrefixes 是 JS fetch 禁止访问的网段（netip 前缀黑名单）。
//
// 修复：原实现只调用 net.IP 的 IsLoopback/IsPrivate/... 系列方法，漏掉了
// CGNAT 100.64.0.0/10（Tailscale 常用）、基准测试 198.18.0.0/15、IETF 协议
// 分配 192.0.0.0/24、保留 240.0.0.0/4、0.0.0.0/8 等；IPv6 转译前缀另行解出
// 内嵌 IPv4 再判（见 developerJSEmbeddedIPv4）。
var developerJSBlockedPrefixes = func() []netip.Prefix {
	raw := []string{
		// IPv4
		"0.0.0.0/8",       // 本网络 / 未指定
		"10.0.0.0/8",      // RFC1918
		"100.64.0.0/10",   // CGNAT / Tailscale
		"127.0.0.0/8",     // 回环
		"169.254.0.0/16",  // 链路本地 / 云元数据
		"172.16.0.0/12",   // RFC1918
		"192.0.0.0/24",    // IETF 协议分配
		"192.0.2.0/24",    // TEST-NET-1
		"192.88.99.0/24",  // 6to4 中继任播（已废弃）
		"192.168.0.0/16",  // RFC1918
		"198.18.0.0/15",   // 基准测试
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // 组播
		"240.0.0.0/4",     // 保留（含 255.255.255.255 广播）
		// IPv6
		"::/96",         // 未指定、回环与已废弃的 IPv4-compatible 地址
		"100::/64",      // 丢弃前缀
		"2001:db8::/32", // 文档
		"fc00::/7",      // ULA
		"fe80::/10",     // 链路本地
		"fec0::/10",     // 站点本地（已废弃）
		"ff00::/8",      // 组播
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, item := range raw {
		out = append(out, netip.MustParsePrefix(item))
	}
	return out
}()

var (
	developerJSNAT64Prefix      = netip.MustParsePrefix("64:ff9b::/96")
	developerJSNAT64LocalPrefix = netip.MustParsePrefix("64:ff9b:1::/48")
	developerJS6to4Prefix       = netip.MustParsePrefix("2002::/16")
	developerJSTeredoPrefix     = netip.MustParsePrefix("2001::/32")
)

// developerJSBlockedAddr 判断地址是否属于禁止访问的内网/保留网段。
// IPv4-mapped（::ffff:a.b.c.d）先还原成 IPv4；NAT64、6to4、Teredo 前缀会解出
// 内嵌的 IPv4 再判一次，避免用 [64:ff9b::a00:1] 之类写法绕到 10.0.0.1。
func developerJSBlockedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap().WithZone("")
	for _, prefix := range developerJSBlockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	if addr.Is6() {
		// 本地使用的 NAT64 前缀（RFC 8215）内嵌位置不固定，整段拒绝。
		if developerJSNAT64LocalPrefix.Contains(addr) {
			return true
		}
		for _, embedded := range developerJSEmbeddedIPv4(addr) {
			if developerJSBlockedAddr(embedded) {
				return true
			}
		}
	}
	return false
}

// developerJSEmbeddedIPv4 解出 IPv6 转译前缀里内嵌的 IPv4 地址。
func developerJSEmbeddedIPv4(addr netip.Addr) []netip.Addr {
	b := addr.As16()
	switch {
	case developerJSNAT64Prefix.Contains(addr):
		return []netip.Addr{netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})}
	case developerJS6to4Prefix.Contains(addr):
		return []netip.Addr{netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})}
	case developerJSTeredoPrefix.Contains(addr):
		// Teredo：第 4-7 字节是服务器 IPv4，最后 4 字节是按位取反的客户端 IPv4。
		server := netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]})
		client := netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]})
		return []netip.Addr{server, client}
	}
	return nil
}
