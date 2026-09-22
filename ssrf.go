package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// validateTarget 校验用户提交的下载地址.
// 默认禁止指向内网/回环/链路本地/CGNAT 地址, 防止这台中转机被当成内网跳板.
func validateTarget(raw string, allowPrivate bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("地址格式不合法")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("只支持 http/https 协议 (当前 %s)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("地址缺少主机名")
	}
	if allowPrivate {
		return u, nil
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("禁止访问内网地址 (%s)", host)
		}
		return u, nil
	}

	// 域名: 解析后逐个校验, 防止 DNS 指向内网
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("域名解析失败: %s", host)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("域名无解析结果: %s", host)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("禁止访问内网地址 (%s -> %s)", host, ip)
		}
	}
	return u, nil
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	// CGNAT 100.64.0.0/10
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		// 保留段 192.0.0.0/24, 198.18.0.0/15, 240.0.0.0/4
		if v4[0] == 192 && v4[1] == 0 && v4[2] == 0 {
			return true
		}
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
		if v4[0] >= 240 {
			return true
		}
	}
	// IPv6 唯一本地地址 fc00::/7
	if len(ip) == net.IPv6len && ip.To4() == nil {
		if ip[0]&0xfe == 0xfc {
			return true
		}
	}
	return false
}
