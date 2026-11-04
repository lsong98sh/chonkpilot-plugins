// 配置页数据面（MEMORY-UI-1，2026-11-02）：
// 记忆沉淀配置从主仓「项目配置 → 上下文」迁移到 memory 插件自身的配置页
// （插件维护 → 记忆沉淀 → 配置）。本文件只做**数据搬运与校验**：
//
//	读：memory.* 项目配置（data-prj-config-list） + 类别清单（data-memory-list）
//	写：data-prj-config-save（entries 批量） / data-memory-* / prompt 域（经 main 的 p.Call）
//
// 键与默认值沿用既有口径（64-配置项一览 / 20-gui §12），**零新增配置键 / 零改形**。
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PrjConfigSaveSubject 是项目配置批量写（persist 订阅；payload `{instance_id, data:{entries}}`）。
const PrjConfigSaveSubject = "data-prj-config-save"

// PresetCategories 是预置项目级类别（与数据层 memoryCategorySpecs 同口径；
// 预置不可删除，仅可清空内容）。用户级「用户偏好」单独由 level=user 标识。
var PresetCategories = []string{
	"项目概要", "共同库", "开发规范", "构建发布规则", "接口库", "测试规范", "典型参照", "用户决策",
}

// IsPresetCategory 判断是否预置类别（配置页据此决定是否显示「删除」）。
func IsPresetCategory(name string) bool {
	for _, c := range PresetCategories {
		if c == name {
			return true
		}
	}
	return false
}

// ConfigView 读回配置页所需的完整快照。
func (p *Plugin) ConfigView(instanceID string) (map[string]any, error) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, errors.New("instance_id required")
	}
	cfg, err := p.resolveConfig(instanceID)
	if err != nil {
		return nil, fmt.Errorf("读取项目配置失败：%w", err)
	}
	cats, total, err := p.categoryViews(instanceID, cfg)
	if err != nil {
		return nil, fmt.Errorf("读取记忆类别失败：%w", err)
	}
	minTokens := cfg.MinTokens
	if minTokens <= 0 {
		minTokens = defaultMinTurnTokens
	}
	return map[string]any{
		"ok":                  true,
		"enabled":             cfg.Enabled,
		"min_turn_tokens":     minTokens,
		"category_max_tokens": cfg.MaxTokens,
		"subsession":          cfg.Subsession,
		"trigger":             cfg.Trigger,
		"safety_tokens":       cfg.SafetyTokens,
		"total_tokens":        total,
		"categories":          cats,
	}, nil
}

// categoryViews 读类别清单并叠加启用态与 token 合计。
func (p *Plugin) categoryViews(instanceID string, cfg memoryConfig) ([]map[string]any, int, error) {
	res, err := p.request(memoryListSubject, map[string]any{"instance_id": instanceID})
	if err != nil {
		return nil, 0, err
	}
	raw, _ := res["list"].([]any)
	out := make([]map[string]any, 0, len(raw))
	total := 0
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		name := strval(m["category"])
		if name == "" {
			continue
		}
		tokens := 0
		switch n := m["tokens"].(type) {
		case float64:
			tokens = int(n)
		case int:
			tokens = n
		case string:
			tokens, _ = strconv.Atoi(strings.TrimSpace(n))
		}
		enabled := cfg.categoryEnabled(name)
		if enabled {
			total += tokens
		}
		out = append(out, map[string]any{
			"category":        name,
			"level":           strval(m["level"]),
			"tokens":          tokens,
			"enabled":         enabled,
			"prompt":          strval(m["prompt"]),
			"prompt_override": m["prompt_override"] == true,
			"preset":          IsPresetCategory(name) || strval(m["level"]) == "user",
		})
	}
	return out, total, nil
}

// SaveConfigEntries 校验并批量写项目配置（一次 data-prj-config-save）。
func (p *Plugin) SaveConfigEntries(instanceID string, entries map[string]string) error {
	if strings.TrimSpace(instanceID) == "" {
		return errors.New("instance_id required")
	}
	if len(entries) == 0 {
		return nil
	}
	norm := make(map[string]any, len(entries))
	for k, v := range entries {
		nv, err := normalizeConfigValue(k, v)
		if err != nil {
			return err
		}
		norm[k] = nv
	}
	_, err := p.request(PrjConfigSaveSubject, map[string]any{
		"instance_id": instanceID,
		"data":        map[string]any{"entries": norm},
	})
	if err != nil {
		return fmt.Errorf("写入项目配置失败：%w", err)
	}
	return nil
}

// normalizeConfigValue 校验单个配置键（只接受 memory.* 闭集）。
func normalizeConfigValue(key, value string) (string, error) {
	v := strings.TrimSpace(value)
	switch key {
	case memoryEnabledKey, memorySubsessionKey:
		if v != "" && v != "true" && v != "false" {
			return "", fmt.Errorf("config_invalid: %s 只接受 true / false（收到 %q）", key, v)
		}
		return v, nil
	case memoryMinTokensKey, memoryCategoryMaxTokensKey, memorySafetyTokensKey:
		if v == "" {
			return v, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return "", fmt.Errorf("config_invalid: %s 只接受非负整数（收到 %q）", key, v)
		}
		return strconv.Itoa(n), nil
	case memoryTriggerKey:
		if v == "" {
			return v, nil
		}
		if v != triggerCompress && v != triggerEveryTurn {
			return "", fmt.Errorf("config_invalid: %s 只接受 compress / every-turn（收到 %q）", key, v)
		}
		return v, nil
	}
	if strings.HasPrefix(key, memoryCategoryPrefix) {
		name := strings.TrimPrefix(key, memoryCategoryPrefix)
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("config_invalid: %s 缺少类别名", key)
		}
		if v != "" && v != "true" && v != "false" {
			return "", fmt.Errorf("config_invalid: %s 只接受 true / false（收到 %q）", key, v)
		}
		return v, nil
	}
	return "", fmt.Errorf("config_invalid: 未知配置键 %s（只接受 memory.*）", key)
}

// CategoryContent 读一个类别的记忆全文（不存在 → 空串）。
func (p *Plugin) CategoryContent(instanceID, category string) (string, error) {
	if strings.TrimSpace(category) == "" {
		return "", errors.New("category required")
	}
	return p.memoryRead(instanceID, category)
}

// SaveCategoryContent 保存一个类别的记忆全文（自定义类别由此创建）。
func (p *Plugin) SaveCategoryContent(instanceID, category, content string) error {
	if strings.TrimSpace(category) == "" {
		return errors.New("category required")
	}
	return p.memorySave(instanceID, category, content)
}

// Flush 手动沉淀一次（复用 memory.flush 订阅体；投递即回）。
func (p *Plugin) Flush(instanceID, session, dialogID string) map[string]any {
	payload, _ := json.Marshal(map[string]any{
		"instance_id": instanceID,
		"session":     session,
		"dialog_id":   dialogID,
	})
	return p.flush(payload)
}
