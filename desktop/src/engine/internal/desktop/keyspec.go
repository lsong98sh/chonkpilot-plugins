package desktop

// 组合键 / VK 规格解析与 IME 切换支持。
//
// KPR/KDN/KUP 支持两类写法：
//   - 单键名：KPR enter、KDN ctrl、KUP alt
//   - 组合键（修饰+主键，+ 分隔）：KPR ctrl+s、KPR alt+f4、KPR shift+enter、KDN win+r
// 修饰名：ctrl/control、alt、shift、win/lwin/meta（0x5B）、rwin（0x5C）。
//
// IME 指令：IME en|off（英文直通，关闭 IME） / IME cn|on（启用中文输入法）。
// 输入路径/ASCII 前用 IME en，避免中文态把 : \ . 转全角。

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// keyModMap 组合修饰名 → VK（ctrl/control/alt/shift/meta 与 win 变体）。
var keyModMap = map[string]uint16{
	"ctrl":    0x11,
	"control": 0x11,
	"shift":   0x10,
	"alt":     0x12,
	"meta":    0x5B,
	"win":     0x5B,
	"lwin":    0x5B,
	"rwin":    0x5C,
}

// parseKeySpec 解析按键规格：返回修饰键 VK 列表（按下顺序）与主键 VK。
// 无 "+" 时整串视为单键名（mods 为空，key=该键）。
func parseKeySpec(spec string) (mods []uint16, key uint16, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, 0, fmt.Errorf("按键为空")
	}
	parts := strings.Split(spec, "+")
	if len(parts) == 1 {
		vk, ok := VKMap[strings.ToLower(strings.TrimSpace(parts[0]))]
		if !ok {
			return nil, 0, fmt.Errorf("未知按键 %s", spec)
		}
		return nil, vk, nil
	}
	for i, p := range parts {
		name := strings.ToLower(strings.TrimSpace(p))
		if i == len(parts)-1 {
			vk, ok := VKMap[name]
			if !ok {
				return nil, 0, fmt.Errorf("未知按键 %s", name)
			}
			return mods, vk, nil
		}
		vk, ok := keyModMap[name]
		if !ok {
			return nil, 0, fmt.Errorf("未知修饰键 %s", name)
		}
		mods = append(mods, vk)
	}
	// 仅修饰无主键（如 ctrl+）不合法
	return nil, 0, fmt.Errorf("组合键缺主键：%s", spec)
}

// ─── IME 切换（imm32）───

var (
	Imm32             = windows.NewLazySystemDLL("imm32.dll")
	ImmGetContext     = Imm32.NewProc("ImmGetContext")
	ImmSetOpenStatus  = Imm32.NewProc("ImmSetOpenStatus")
	ImmReleaseContext = Imm32.NewProc("ImmReleaseContext")
)

// setImeOpen 对前台窗口设置 IME 开关：open=true 启用输入法（中文）；false 关闭（英文直通）。
func setImeOpen(open bool) error {
	ret, _, _ := GetForegroundWindow.Call()
	hwnd := ret
	if hwnd == 0 {
		return nil // 无前台窗口：无操作
	}
	imc, _, _ := ImmGetContext.Call(hwnd)
	if imc == 0 {
		return nil
	}
	defer ImmReleaseContext.Call(hwnd, imc)
	status := uintptr(0)
	if open {
		status = 1
	}
	ok, _, _ := ImmSetOpenStatus.Call(imc, status)
	if ok == 0 {
		return fmt.Errorf("ImmSetOpenStatus failed")
	}
	return nil
}

// imeArg 解析 IME 指令参数：en|off → false（英文）；cn|on|zh → true（中文）。
func imeArg(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "en", "off", "ascii":
		return false, nil
	case "cn", "on", "zh", "chinese":
		return true, nil
	}
	return false, fmt.Errorf("IME 参数须为 en|off（英文）或 cn|on（中文），得到 %q", s)
}
