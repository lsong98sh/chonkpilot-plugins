// ssrf.go —— web_download 的出站目标校验（P0，2026-11-08）。
//
// 默认拒绝 loopback / 私网 / 链路本地 / 未指定 / 组播 / 保留网段，以及 localhost、
// *.localhost、*.local、*.internal、*.lan、*.home.arpa 和单标签主机名；
// 域名目标按 DNS 解析结果逐个校验（防 DNS 指向内网）。
//
// 配置 `plugins.web.allow-private`（默认 false，配置页「允许访问内网」）开启后放行内网。
// 重定向每一跳由 download.go 的 CheckRedirect 重新调用本校验。
package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// extraBlockedNets 是 net.IP 自带判定（IsPrivate/IsLoopback/IsLinkLocal*/IsUnspecified/IsMulticast）
// 未覆盖的非公网网段。
var extraBlockedNets = mustParseCIDRs(
	"0.0.0.0/8",
	"100.64.0.0/10",   // CGNAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved
	"2001:db8::/32",   // documentation
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("ssrf: 非法 CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}

// isBlockedIP 判断地址是否属于禁止访问的非公网范围。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, n := range extraBlockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isBlockedHostname 判断主机名是否明确指向本机/内网。
func isBlockedHostname(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || h == "localhost" {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	// 单标签主机名（如 http://intranet/）几乎都是内网目标。
	if !strings.Contains(h, ".") {
		return true
	}
	return false
}

// checkURLNotPrivate 校验一个 http(s) URL 是否指向公网（allowPrivate=false 时）。
// 域名会解析 DNS 并逐个检查结果；解析失败按失败关闭处理。
func checkURLNotPrivate(ctx context.Context, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("web_download 安全限制：URL 为空")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("web_download 安全限制：只允许 http/https（收到 %q）", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("web_download 安全限制：URL 缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return blockedTargetError(host)
		}
		return nil
	}
	if isBlockedHostname(host) {
		return blockedTargetError(host)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("web_download 安全限制：目标主机 %s 解析失败（如确需访问内网，请在 插件 → 联网能力 开启「允许访问内网」）：%w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("web_download 安全限制：目标主机 %s 未解析到地址", host)
	}
	for _, a := range addrs {
		if isBlockedIP(a.IP) {
			return blockedTargetError(host)
		}
	}
	return nil
}

func blockedTargetError(host string) error {
	return fmt.Errorf("web_download 安全限制：目标 %q 属于本机/内网/链路本地地址，已拒绝；如确需访问请在 插件 → 联网能力 开启「允许访问内网」", host)
}
