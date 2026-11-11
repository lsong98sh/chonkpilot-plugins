package main

import "testing"

func TestValidateVftsQueryArgs(t *testing.T) {
	out, err := validateVftsQueryArgs(map[string]any{"match": "  全文索引  "})
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if out["scope"] != "file" || out["topK"] != 20 || out["match"] != "全文索引" {
		t.Fatalf("default=%v", out)
	}

	out, err = validateVftsQueryArgs(map[string]any{
		"scope": "SESSION", "role": " USER ", "query": "fox AND brown", "topK": 1000,
		"session_id": " s1 ", "path": "docs", "workdir": " E:/p ",
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if out["scope"] != "session" || out["role"] != "user" || out["topK"] != 100 ||
		out["session_id"] != "s1" || out["path"] != "docs" || out["workdir"] != "E:/p" {
		t.Fatalf("session=%v", out)
	}

	for name, args := range map[string]map[string]any{
		"未知参数":      {"match": "x", "foo": 1},
		"都缺":        {},
		"同给":        {"match": "x", "query": "y"},
		"scope 非法":   {"match": "x", "scope": "files"},
		"role 非法":    {"match": "x", "role": "system"},
		"topK 0":     {"match": "x", "topK": 0},
		"topK 小数":    {"match": "x", "topK": 1.5},
		"topK 字符串":   {"match": "x", "topK": "20"},
		"match 类型":    {"match": 1},
		"session 类型":  {"match": "x", "session_id": 1},
	} {
		if _, err := validateVftsQueryArgs(args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}