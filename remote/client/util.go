package main

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

// randomHex 生成 n 字节随机数的十六进制串（2n 个字符）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		s := strconv.FormatInt(time.Now().UnixNano(), 16)
		for len(s) < 2*n {
			s = "0" + s
		}
		return s[len(s)-2*n:]
	}
	return hex.EncodeToString(b)
}

// asMap 把任意 JSON 值断言为对象（非对象 → nil）。
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// asList 把任意 JSON 值断言为数组（非数组 → nil）。
func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// putIf 仅在值非空时写入键（避免把空串发给宿主 / Server）。
func putIf(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}
