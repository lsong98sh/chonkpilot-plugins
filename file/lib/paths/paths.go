// Package paths 提供路径字符串解析的统一入口（决策 R-11 二次升级）：
// 工具/DSL 参数用 ResolvePath（唯一入口，相对路径拒绝）；
// CLI 的 --work-dir/--data-dir 用 ResolveDir（相对路径合法，历史语义不变）。
// 规范见 docs/spec/10-architecture/16-路径解析规范.md。
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNoInstance 表示缺少 instance（宿主未注入调用上下文，决策 R-11 二次升级）：
// 无法解析 !/ 临时目录、也无法注入 DSL env。instance 不应为空——为空即异常。
var ErrNoInstance = errors.New("缺少 instance（宿主未注入调用上下文），无法解析 !/ 或注入 env")

// ExpandHome 把 ~ / ~/x / ~\x 前缀展开为用户 home；~foo（非 ~/ 前缀）不展开，按字面处理。
func ExpandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if len(p) >= 2 && p[0] == '~' && (p[1] == '/' || p[1] == '\\') {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// ─── 临时目录根（!/ 前缀的落地目录，按 instance 分目录）───
//
// 2026-09-19（实例隔离第一批，缺口 4）：临时根由**进程级单值**改为**按 instance 分桶**
// （tempRoots：instanceID → 根），并新增 TempRootFor / ResolvePathFor 两个 instance 显式入口。
// 旧入口语义保持不变：SetTempRoot 仍返回该 instance 的根，并把「当前根」更新为它
// （单实例进程 = 与旧行为逐字节等价）；TempRoot/ResolvePath 仍读「当前根」。
// 多 instance 同进程（split 服务端 exe）的调用点须改用 TempRootFor/ResolvePathFor（按 instance 取），
// 否则「当前根」会被并发 instance 互相覆盖。

var (
	tempMu    sync.RWMutex
	tempRoot  string            // 兼容：最近一次 SetTempRoot 的根（TempRoot/ResolvePath 读它）
	tempRoots map[string]string // instanceID（已 sanitize）→ 根（按 instance 分桶）
)

// tempRootOf 计算并登记某 instance 的临时根（不动「当前根」）：
//
//	<系统 temp>/chonkpilot/<sanitize(instanceID)>
//
// 目录按需创建（创建失败不阻断，返回路径仍可用）。instanceID 为空/平凡 → ("", ErrNoInstance)。
func tempRootOf(instanceID string) (string, error) {
	id := sanitizeInstance(instanceID)
	if id == "" {
		return "", ErrNoInstance
	}
	root := filepath.Join(os.TempDir(), "chonkpilot", id)
	tempMu.Lock()
	if tempRoots == nil {
		tempRoots = make(map[string]string)
	}
	tempRoots[id] = root
	tempMu.Unlock()
	_ = os.MkdirAll(root, 0o755)
	return root, nil
}

// SetTempRoot 设置/刷新临时目录根并返回之（线程安全、幂等）：
//
//	<系统 temp>/chonkpilot/<sanitize(instanceID)>
//
// 目录按需创建（创建失败不阻断，返回路径仍可用）。instanceID 为空 → ("", ErrNoInstance)
// ——不再回落 "default"（instance 为空即异常，决策 R-11 二次升级）。executor 进程启动时以
// 宿主注入的 CHONKPILOT_INSTANCE 调用；server 侧 llm_run 以轮次 instance 调用。
// 语义（2026-09-19 起）：同时登记该 instance 的专属根（TempRootFor 可查），并把「当前根」
// 更新为它（单实例进程沿用旧语义）。
func SetTempRoot(instanceID string) (string, error) {
	root, err := tempRootOf(instanceID)
	if err != nil {
		return "", err
	}
	tempMu.Lock()
	tempRoot = root
	tempMu.Unlock()
	return root, nil
}

// TempRootFor 返回**指定 instance** 的临时目录根（按 instance 分桶；多实例进程的调用点用它，
// 避免各 instance 互相覆盖）。无需先 SetTempRoot：根由 instance 确定性推导并登记；
// instanceID 为空/平凡 → ("", ErrNoInstance)。
func TempRootFor(instanceID string) (string, error) {
	id := sanitizeInstance(instanceID)
	if id == "" {
		return "", ErrNoInstance
	}
	tempMu.RLock()
	root := tempRoots[id]
	tempMu.RUnlock()
	if root != "" {
		return root, nil
	}
	return tempRootOf(instanceID)
}

// TempRoot 返回当前临时目录根；未设置（从未 SetTempRoot 或 instance 为空）→ ("", ErrNoInstance)。
func TempRoot() (string, error) {
	tempMu.RLock()
	r := tempRoot
	tempMu.RUnlock()
	if r == "" {
		return "", ErrNoInstance
	}
	return r, nil
}

// returnFilePrefix / returnFileSuffix 界定 `$RETURN` file 态落盘文件（宿主生成 dsl-return-<id>.md）。
const (
	returnFilePrefix = "dsl-return-"
	returnFileSuffix = ".md"
)

// SweepStaleReturnFiles 清理临时根下**陈旧**的 `$RETURN` 落盘文件（dsl-return-*.md，mtime 早于
// now-maxAge），返回删除数。用于作业启动兜底：常驻进程下 `$RETURN` file 态（>64K 结果）按作业
// 线性累积且无其它删除路径（取消路径的残留也由此在下次作业时清掉）。**仅删陈旧文件** —— 保留
// 近期结果（仍可被上层读取），故不会造成数据丢失。root 为空 / maxAge<=0 → no-op。
func SweepStaleReturnFiles(root string, maxAge time.Duration) int {
	if root == "" || maxAge <= 0 {
		return 0
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, returnFilePrefix) || !strings.HasSuffix(name, returnFileSuffix) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(root, name)) == nil {
			n++
		}
	}
	return n
}

// sanitizeInstance 清洗 instance id（仅保留字母/数字/-/_/.，其余替换为 _）；空/平凡值 → ""。
func sanitizeInstance(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return ""
	}
	return out
}

// trimTempPrefix 识别 !/ 或 !\ 前缀（临时目录），返回其后的相对部分。
func trimTempPrefix(raw string) (string, bool) {
	if strings.HasPrefix(raw, "!/") || strings.HasPrefix(raw, `!\`) {
		return strings.TrimLeft(raw[2:], `/\`), true
	}
	return "", false
}

// joinTempRoot 把 !/ 之后的相对部分接到临时根下；`..` 越出根（Clean 后不再以根为前缀）
// → ok=false（拒绝 `!/../../x` 之类的穿越）。
func joinTempRoot(root, rest string) (string, bool) {
	root = filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(root, rest))
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", false
	}
	return joined, true
}

// ─── 唯一解析入口（R-11 严格语义）───

// ResolvePath 是工具/DSL 参数路径解析的唯一入口（R-11 二次升级）。规则顺序：
//
//  1. raw == ""               → ("", "")（空值交给调用方做必填校验）
//  2. !/ 或 !\ 前缀           → <tempRoot>/...（tempRoot = SetTempRoot/TempRoot，按 instance 分目录）；
//     `..` 越出 tempRoot → 统一错误消息（拒绝穿越）
//  3. ~ / ~/x / ~\x           → 展开用户 home（ExpandHome；~foo 不展开）
//  4. filepath.IsAbs          → filepath.Clean 原样
//  5. 其余：base != "" → filepath.Clean(Join(base, raw))（**仅供 CLI 参数解析**）；
//     base == "" → 返回统一错误消息（相对路径拒绝，见 InvalidPathMessage）
//
// 返回 (绝对/展开后路径, 错误消息)；错误消息非空表示不合规。
func ResolvePath(raw, base string) (string, string) {
	if raw == "" {
		return "", ""
	}
	if rest, ok := trimTempPrefix(raw); ok {
		root, err := TempRoot()
		if err != nil {
			return "", err.Error() // 缺 instance → 统一错误消息（不 panic）
		}
		p, ok := joinTempRoot(root, rest)
		if !ok {
			return "", InvalidPathMessage(raw) // `!/` 内 `..` 越出临时根 → 拒绝
		}
		return p, ""
	}
	s := ExpandHome(raw)
	if filepath.IsAbs(s) {
		return filepath.Clean(s), ""
	}
	if base != "" {
		return filepath.Clean(filepath.Join(base, s)), ""
	}
	return "", InvalidPathMessage(raw)
}

// InvalidPathMessage 构造路径不合规的统一错误消息（单一实现，含原值 + 允许的三种前缀写法与
// 示例 + DSL 场景提示）。供 ResolvePath 与各工具（mcp-tools / llm_run）复用。
func InvalidPathMessage(raw string) string {
	return "路径必须是绝对路径、以 ~/ 开头的用户目录、或以 !/ 开头的临时目录：" + raw +
		"（例：C:\\work\\proj\\src\\main.py、~/data/x.txt、!/tmp.csv；DSL 内可用 {{env.CHONKPILOT_WORKDIR}} 拼项目内路径）"
}

// ResolvePathFor 是 ResolvePath 的 **instance 显式**版本（2026-09-19，缺口 4）：
// 语义与错误消息完全一致，唯一差别是 `!/` 前缀落到**指定 instance** 的临时根
// （TempRootFor(instanceID)），而非进程「当前根」——多 instance 同进程（split 服务端 exe）专用，
// 避免并发 instance 互相覆盖。非 `!/` 路径与 instance 无关 → 直接委托 ResolvePath。
func ResolvePathFor(instanceID, raw, base string) (string, string) {
	if raw == "" {
		return "", ""
	}
	if rest, ok := trimTempPrefix(raw); ok {
		root, err := TempRootFor(instanceID)
		if err != nil {
			return "", err.Error() // 缺 instance → 统一错误消息（不 panic）
		}
		p, ok := joinTempRoot(root, rest)
		if !ok {
			return "", InvalidPathMessage(raw) // `!/` 内 `..` 越出临时根 → 拒绝
		}
		return p, ""
	}
	return ResolvePath(raw, base)
}

// ─── CLI 专用（历史语义，不参与 R-11 收紧）───

// ResolveDir 是 CLI 目录字符串解析的唯一入口（--work-dir/--data-dir）：
//
//  1. ~ / ~/x / ~\x 前缀 → 展开为用户 home（ExpandHome）
//  2. filepath.IsAbs（盘符 c:\、C:/、\、/、UNC \\）→ 原样 filepath.Clean
//  3. 其余（含 data、./data、../data）→ filepath.Clean(Join(base, raw))，可 ../ 跳出 base
//
// raw 为空返回空（由调用方决定缺省值）；base 为空时相对路径按字面返回（Clean 后）。
// 行为与 R-11 前完全一致（相对路径合法、base 为空原样），不接受 !/ 前缀。
func ResolveDir(raw, base string) string {
	if raw == "" {
		return ""
	}
	s := ExpandHome(raw)
	if filepath.IsAbs(s) {
		return filepath.Clean(s)
	}
	return filepath.Clean(filepath.Join(base, s))
}

// ─── DB 逻辑路径（G-24；16 §7/§8）───
//
// 口径：**落库只存逻辑路径（相对 workdir，斜杠归一）**，边界双向转换 ——
// 落库前 ToLogical（反展开），调用/展示前 FromLogical（展开）。
// 动机：三区部署下同一 workdir 在不同区路径不同（存储区 /projects vs 应用区
// /home/chonkpilot/projects），绝对路径原样落库 → 换挂载布局 / 迁移后历史引用全部失效。
//
// 适用面 = 与某具体 workdir 绑定的个人运行态（当前：`opened-files`）。
// **不适用**：`recent_dirs`（内容就是各 workdir 根，无法相对自身）；
// `filetree-*`（已按 workdir 相对落库）。workdir 之外的路径保持原样。

// isFilePath 判定是否为可展开的文件路径（排除 `db://`、`http://` 等带 scheme 的引用；
// 文件路径（含 Windows 盘符）不会出现 `://`）。
func isFilePath(p string) bool {
	return !strings.Contains(p, "://")
}

// ToLogical 把绝对路径归一为落库用逻辑路径（反展开）：
//   - 空串 / 带 scheme（`db://` 等）/ workdir 为空 → 原样；
//   - 非绝对（已是逻辑相对路径）→ 斜杠归一后原样；
//   - workdir 内（含 workdir 本身）→ 相对 workdir 的斜杠路径（workdir 本身 → ""）；
//   - workdir 之外（`..` 越界）→ 原样绝对路径（不产出 `../` 逻辑路径）。
func ToLogical(workdir, p string) string {
	if p == "" || workdir == "" || !isFilePath(p) {
		return p
	}
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(p)
	}
	rel, err := filepath.Rel(filepath.Clean(workdir), filepath.Clean(p))
	if err != nil {
		return p
	}
	if rel == "." {
		return ""
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p // 越出 workdir → 保持绝对
	}
	return filepath.ToSlash(rel)
}

// FromLogical 把落库逻辑路径还原为绝对路径（展开）：
//   - 空串 / 带 scheme（`db://` 等）→ 原样；
//   - 绝对路径（旧数据 / workdir 外）→ 原样；
//   - 相对路径 → Join(workdir, rel)（workdir 为空 → 原样）。
func FromLogical(workdir, p string) string {
	if p == "" || !isFilePath(p) {
		return p
	}
	if filepath.IsAbs(p) {
		return p
	}
	if workdir == "" {
		return p
	}
	return filepath.Join(workdir, filepath.FromSlash(p))
}
