// fsops.go — file_delete / file_move / file_copy。
package filetool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-util/safepath"
)

// 测试钩子：注入 rename / removeAll 的平台错误（生产 = os.Rename / os.RemoveAll）。
var (
	renameFn    = os.Rename
	removeAllFn = os.RemoveAll
)

// 独占打开相关的稳定错误（delete 与 write/replace/patch 共用；平台实现在
// exclusive_*.go / open_exclusive_*.go 中映射）。errFileInUse 必须保持包级可见。
var (
	errFileInUse    = errors.New("file is in use by another process")
	errAccessDenied = errors.New("access denied")
)

// maxDeleteFiles 是单个 target 的目录树/文件条目数上限（不跨 target 合计）。
// 超过上限时该项 failed，不持有句柄、不删除，提示调用方分批删除子目录。
const maxDeleteFiles = 1000

// deleteHoldRetryDelays 是第一遍未拿到独占句柄后的指数退避间隔。
var deleteHoldRetryDelays = []time.Duration{
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
}

// runDelete 实现 file_delete。
func runDelete(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	seen := map[string]bool{}
	for i, t := range req.targets {
		path, perr := resolvePath(asString(t, "path"), fmt.Sprintf("targets[%d].path", i))
		if perr != "" {
			results = append(results, itemErr(map[string]any{"path": asString(t, "path")}, perr))
			continue
		}
		if seen[path] {
			results = append(results, itemErr(map[string]any{"path": path}, "duplicate target"))
			continue
		}
		seen[path] = true
		results = append(results, deleteOne(path, t))
	}
	return finish(tool, results)
}

// deleteOne 删除单个文件、目录或符号链接。
//
// 类型判定用 Lstat（不跟随链接）：symlink 只删除链接本身；目录必须显式
// recursive=true；目录树先做一次 WalkDir 统计与普通文件收集（上限
// maxDeleteFiles）。
//
// Windows：对收集到的普通文件全部持有独占句柄（第一遍冲突跳过、3 轮退避重试），
// 全部拿到后逐个标记 delete-on-close，再关闭句柄完成删除；随后用 os.Remove
// 删除 symlink 与空目录（自底向上，避免误删预检后新增的文件）。
// 非 Windows：保留「预检 no-op + RemoveAll」路径。
func deleteOne(path string, t map[string]any) map[string]any {
	out := map[string]any{"path": path}
	if reason := unsafeRoot(path); reason != "" {
		return itemErr(out, reason)
	}
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) && asBool(t, "missingOk", false) {
			return itemSkip(out, "not found")
		}
		return itemErr(out, err.Error())
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		// 只删除链接本身：不跟随目标、不要求 recursive、不计算 md5/size、不持有句柄。
		out["type"] = "symlink"
		if target, rerr := os.Readlink(path); rerr == nil {
			out["link_target"] = target
		} else {
			out["link_target_error"] = rerr.Error()
		}
		if err := os.Remove(path); err != nil {
			return itemErr(out, err.Error())
		}
		return itemOK(out)
	case st.IsDir():
		out["type"] = "dir"
		if !asBool(t, "recursive", false) {
			return itemErr(out, "directory deletion requires recursive=true")
		}
		scan, serr := scanDeleteTree(path)
		if serr != nil {
			return itemErr(out, "scan: "+serr.Error())
		}
		if scan.over {
			return itemErr(out, fmt.Sprintf("too many files (%d > %d); delete subdirectories in batches", scan.files, maxDeleteFiles))
		}
		out["removedFiles"] = scan.files
		out["removedBytes"] = scan.bytes
		if !holdDeleteSemantics {
			// 非 Windows：探测为 no-op，保持原有 RemoveAll 语义。
			if err := removeAllFn(path); err != nil {
				out["partial"] = true
				return itemErr(out, "remove failed (pre-scan counts may include entries not deleted): "+err.Error())
			}
			return itemOK(out)
		}
		held, herr := holdAll(scan.regular, nil)
		if herr != nil {
			return holdFailureResult(out, herr)
		}
		partial, cerr := commitHeldDelete(held)
		if cerr != nil {
			if partial {
				out["partial"] = true
			}
			return itemErr(out, "delete-on-close commit failed: "+cerr.Error())
		}
		if rerr := removeLinksAndDirs(scan); rerr != nil {
			out["partial"] = true
			return itemErr(out, "files deleted; some links or directories remain: "+rerr.Error())
		}
		return itemOK(out)
	default:
		out["type"] = "file"
		out["size"] = st.Size()
		if !holdDeleteSemantics {
			if sum, merr := md5File(path); merr == nil {
				out["md5"] = sum
			} else {
				out["md5_skipped"] = "unreadable"
			}
			if err := removeAllFn(path); err != nil {
				return itemErr(out, err.Error())
			}
			return itemOK(out)
		}
		// 单文件：同一套「持有 + 重试」，md5 必须从持有的句柄读取。
		held, herr := holdAll([]string{path}, map[string]bool{path: true})
		if herr != nil {
			return holdFailureResult(out, herr)
		}
		if sum, merr := held[0].md5(); merr == nil {
			out["md5"] = sum
		} else {
			out["md5_skipped"] = "unreadable"
		}
		partial, cerr := commitHeldDelete(held)
		if cerr != nil {
			if partial {
				out["partial"] = true
			}
			return itemErr(out, "delete-on-close commit failed: "+cerr.Error())
		}
		return itemOK(out)
	}
}

// deleteScan 是一次目录遍历的统计结果。
type deleteScan struct {
	files   int      // 非目录条目数（含 symlink）
	bytes   int64    // 普通文件与 symlink 自身大小之和
	regular []string // 需要持有独占句柄的普通文件路径
	links   []string // symlink 路径（文件删除后用 os.Remove 删链接）
	others  []string // 其他非目录、非普通文件条目（os.Remove）
	dirs    []string // 目录（含 root，先序；删除时逆序）
	over    bool     // 是否已超过 maxDeleteFiles
}

// scanDeleteTree 用一次 WalkDir 统计条目并收集普通文件/symlink/目录路径
// （不跟随目录 symlink）。
//
// d.IsDir() 基于 Lstat，目录 symlink 不会被当作目录继续下钻；
// 条目数超过 maxDeleteFiles 时立即停止遍历并置 over。
func scanDeleteTree(root string) (*deleteScan, error) {
	s := &deleteScan{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			s.dirs = append(s.dirs, p)
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		s.files++
		if s.files > maxDeleteFiles {
			s.over = true
			return filepath.SkipAll
		}
		switch {
		case info.Mode().IsRegular():
			s.bytes += info.Size()
			s.regular = append(s.regular, p)
		case info.Mode()&os.ModeSymlink != 0:
			// symlink 计入 files/bytes，但不持有句柄、不跟随目标。
			s.bytes += info.Size()
			s.links = append(s.links, p)
		default:
			s.others = append(s.others, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// holdError 描述一次独占持有失败。
type holdError struct {
	path  string
	code  string
	err   error
	files []string // 重试结束后仍被占用的全部路径（FILE_IN_USE）
}

func holdErrCode(err error) string {
	switch {
	case errors.Is(err, errFileInUse):
		return "FILE_IN_USE"
	case errors.Is(err, errAccessDenied):
		return "ACCESS_DENIED"
	default:
		return "HOLD_FAILED"
	}
}

// holdAll 建立全部文件的独占句柄：
//   - 第一遍逐个尝试，共享/锁冲突记入 pending 并继续处理后续文件；
//   - 3 轮指数退避重试（50/100/200ms），每轮重试全部仍 pending 的文件；
//   - 仍有 pending 或遇到 fatal 错误时关闭全部已持有句柄（不标记 delete），
//     一个都不删。
func holdAll(paths []string, forRead map[string]bool) ([]*heldFile, *holdError) {
	held := make([]*heldFile, 0, len(paths))
	pending := make([]string, 0)
	for _, p := range paths {
		h, err := holdExclusive(p, forRead[p])
		if err == nil {
			held = append(held, h)
			continue
		}
		if errors.Is(err, errFileInUse) {
			pending = append(pending, p)
			continue
		}
		_ = closeHeld(held)
		return nil, &holdError{path: p, code: holdErrCode(err), err: err}
	}
	for _, delay := range deleteHoldRetryDelays {
		if len(pending) == 0 {
			break
		}
		time.Sleep(delay)
		still := pending[:0]
		for _, p := range pending {
			h, err := holdExclusive(p, forRead[p])
			if err == nil {
				held = append(held, h)
				continue
			}
			if errors.Is(err, errFileInUse) {
				still = append(still, p)
				continue
			}
			_ = closeHeld(held)
			return nil, &holdError{path: p, code: holdErrCode(err), err: err}
		}
		pending = still
	}
	if len(pending) > 0 {
		_ = closeHeld(held)
		return nil, &holdError{
			path:  pending[0],
			code:  "FILE_IN_USE",
			err:   errFileInUse,
			files: append([]string(nil), pending...),
		}
	}
	return held, nil
}

func closeHeld(held []*heldFile) error {
	var first error
	for _, h := range held {
		if err := h.close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// commitHeldDelete 标记全部 delete-on-close 后关闭句柄；任一标记失败则清除已标记，
// 关闭全部且不删除。partial=true 表示清除失败，关闭时可能已删除部分文件。
func commitHeldDelete(held []*heldFile) (partial bool, err error) {
	marked := make([]*heldFile, 0, len(held))
	for _, h := range held {
		if merr := h.markDeleteOnClose(); merr != nil {
			var clearErr error
			for _, m := range marked {
				if cerr := m.clearDeleteOnClose(); cerr != nil && clearErr == nil {
					clearErr = cerr
				}
			}
			_ = closeHeld(held)
			if clearErr != nil {
				return true, fmt.Errorf("mark delete-on-close failed: %w; clearing marked handles failed: %v", merr, clearErr)
			}
			return false, fmt.Errorf("mark delete-on-close failed: %w", merr)
		}
		marked = append(marked, h)
	}
	if cerr := closeHeld(held); cerr != nil {
		return true, fmt.Errorf("close after delete-on-close failed: %w", cerr)
	}
	return false, nil
}

// removeLinksAndDirs 在文件句柄关闭（文件已删）后清理 symlink 与其他非目录条目，
// 并自底向上删除空目录。使用 os.Remove 而非 RemoveAll：目录非空（例如预检后
// 新出现的文件）时保留残留并返回错误，不会静默删除新文件。
func removeLinksAndDirs(scan *deleteScan) error {
	var first error
	try := func(p string) {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	for _, p := range scan.links {
		try(p)
	}
	for _, p := range scan.others {
		try(p)
	}
	for i := len(scan.dirs) - 1; i >= 0; i-- {
		try(scan.dirs[i])
	}
	return first
}

// holdFailureResult 把持有失败映射为 failed 项（code 区分 FILE_IN_USE / ACCESS_DENIED）。
func holdFailureResult(out map[string]any, he *holdError) map[string]any {
	out["code"] = he.code
	out["locked_path"] = he.path
	if len(he.files) > 0 {
		out["locked_files"] = he.files
	}
	switch he.code {
	case "FILE_IN_USE":
		return itemErr(out, "file is in use by another process: "+he.path)
	case "ACCESS_DENIED":
		return itemErr(out, "access denied: "+he.path)
	default:
		return itemErr(out, "failed to hold exclusive handle: "+he.path+": "+he.err.Error())
	}
}

// unsafeRoot 拒绝删除文件系统根 / 盘符根 / 用户主目录根。
func unsafeRoot(path string) string {
	return safepath.RefuseUnsafeRoot(path)
}

// countFiles 统计目录树下的文件数（删除前调用，供 removedFiles 审计字段）。
func countFiles(root string) int {
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// runMove 实现 file_move。
func runMove(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	seenFrom := map[string]bool{}
	seenTo := map[string]bool{}
	for i, t := range req.targets {
		from, ferr := resolvePath(asString(t, "from"), fmt.Sprintf("targets[%d].from", i))
		to, terr := resolvePath(asString(t, "to"), fmt.Sprintf("targets[%d].to", i))
		if ferr != "" || terr != "" {
			results = append(results, itemErr(map[string]any{
				"from": asString(t, "from"), "to": asString(t, "to"),
			}, firstNonEmpty(ferr, terr)))
			continue
		}
		if seenTo[to] {
			results = append(results, itemErr(map[string]any{"from": from, "to": to}, "duplicate target"))
			continue
		}
		if seenFrom[from] {
			results = append(results, itemErr(map[string]any{"from": from, "to": to}, "duplicate source"))
			continue
		}
		seenTo[to] = true
		seenFrom[from] = true
		results = append(results, moveOne(from, to, t))
	}
	return finish(tool, results)
}

// moveOne 移动/重命名单个目标。
//
// 顺序：same_path → 自我嵌套 → Stat 源 → createDirs → Lstat 判存在/类型
// → 目录合并不是 rename 可表达的，直接走复制合并；否则先 rename，仅 EXDEV 才回退复制。
// rename 的其他错误（权限/占用等）不复制、不删源，原文案以 `rename:` 前缀返回。
//
// 不再使用 `<to>.chonk.lock`：同卷 rename 由 OS 原子完成；overwrite=false 的
// 存在性检查与 rename 之间是无锁的 best-effort（外部进程竞争可能赢，契约已注明）。
func moveOne(from, to string, t map[string]any) map[string]any {
	out := map[string]any{"from": from, "to": to}
	// 同一路径 = 无操作；Windows 大小写不敏感、分隔符归一，非 Windows 精确比较。
	if samePath(from, to) {
		return itemSkip(out, "same_path")
	}
	if isWithin(to, from) {
		return itemErr(out, "target is inside source (self-nested move)")
	}
	st, err := os.Stat(from)
	if err != nil {
		return itemErr(out, err.Error())
	}
	out["type"] = typeName(st)
	overwrite := asBool(t, "overwrite", false)
	if asBool(t, "createDirs", true) {
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return itemErr(out, "create dir: "+err.Error())
		}
	}

	var toInfo os.FileInfo
	toExisted := false
	if fi, statErr := os.Lstat(to); statErr == nil {
		toInfo = fi
		toExisted = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return itemErr(out, "stat: "+statErr.Error())
	}
	if toExisted && !overwrite {
		return itemErr(out, "target exists (overwrite=false)")
	}
	if toExisted && st.IsDir() != toInfo.IsDir() {
		return itemErr(out, "target type mismatch (source and target must both be file or both be dir)")
	}

	// 目录合并到已存在目标：rename 无法合并，直接复制合并后删源。
	if st.IsDir() && toExisted {
		return moveByCopy(out, from, to, st, overwrite, toExisted)
	}

	// 同卷 rename；目录文件数必须在 rename 前统计（rename 后源已不存在）。
	dirFiles := 0
	if st.IsDir() {
		dirFiles = countFiles(from)
	}
	if err := renameFn(from, to); err == nil {
		if st.IsDir() {
			out["files"] = dirFiles
		} else {
			out["bytes"] = st.Size()
		}
		return itemOK(out)
	} else if !errors.Is(err, syscall.EXDEV) {
		// 非跨卷错误：可能是权限/占用/目标类型问题，绝不复制或删源。
		return itemErr(out, "rename: "+err.Error())
	}
	return moveByCopy(out, from, to, st, overwrite, toExisted)
}

// moveByCopy 执行「复制成功后删源」，并显式描述中间态：
//
//   - 复制失败且目标原本不存在 → 回滚本次新建的目标树，源保持完整；
//   - 复制失败且目标原本存在（目录合并或 Windows 就地覆盖文件）→ partial + copiedFiles，
//     目标可能部分更新、源未动；
//   - 复制成功但删源失败 → partial + source_removed:false，目标完整、源未删除。
func moveByCopy(out map[string]any, from, to string, st os.FileInfo, overwrite, toExisted bool) map[string]any {
	stats, cerr := copyPath(from, to, overwrite)
	if cerr != nil {
		if st.IsDir() && !toExisted {
			// 目标树完全由本次调用新建：整体回滚，恢复到调用前（不存在）的状态。
			if rerr := removeAllFn(to); rerr != nil {
				return itemErr(out, "copy failed: "+cerr.Error()+"; rollback failed: "+rerr.Error())
			}
		} else if copyFailurePartial(stats, toExisted, st, cerr) {
			// 合并写入已有目标：不能删除用户已有数据，标记 partial 并给出已复制文件数。
			out["partial"] = true
			if st.IsDir() {
				out["copiedFiles"] = stats.files
			}
		}
		code, msg := copyFailureMessage(cerr, to)
		if code != "" {
			out["code"] = code
		}
		return itemErr(out, "copy failed: "+msg)
	}
	out["bytes"] = stats.bytes
	if st.IsDir() {
		out["files"] = stats.files
	}
	if rerr := removeAllFn(from); rerr != nil {
		out["partial"] = true
		out["source_removed"] = false
		out["copiedFiles"] = stats.files
		return itemErr(out, "copied but failed to remove source: "+rerr.Error())
	}
	return itemOK(out)
}

// runCopy 实现 file_copy。
func runCopy(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	seen := map[string]bool{}
	for i, t := range req.targets {
		from, ferr := resolvePath(asString(t, "from"), fmt.Sprintf("targets[%d].from", i))
		to, terr := resolvePath(asString(t, "to"), fmt.Sprintf("targets[%d].to", i))
		if ferr != "" || terr != "" {
			results = append(results, itemErr(map[string]any{
				"from": asString(t, "from"), "to": asString(t, "to"),
			}, firstNonEmpty(ferr, terr)))
			continue
		}
		if seen[to] {
			results = append(results, itemErr(map[string]any{"from": from, "to": to}, "duplicate target"))
			continue
		}
		seen[to] = true
		results = append(results, copyOne(from, to, t))
	}
	return finish(tool, results)
}

// copyOne 复制单个目标。
//
// 顺序：createDirs → Lstat 判存在/类型 → overwrite 判定 → 复制。
// 目录复制是**合并**语义（不删除目标中多出的文件）；
// 目录复制中途失败时：目标原本不存在 → 回滚新建的目标树；目标原本存在 → 返回
// partial+copiedFiles，保留已复制内容，便于调用方判断真实状态。
// 文件复制失败时：Windows 就地覆盖已存在目标 → partial；目标原本不存在 → 平台实现
// 删除本次新建的空/半截目标。不再使用 `<to>.chonk.lock`。
func copyOne(from, to string, t map[string]any) map[string]any {
	out := map[string]any{"from": from, "to": to}
	if isWithin(to, from) {
		return itemErr(out, "target is inside source (self-nested copy)")
	}
	st, err := os.Stat(from)
	if err != nil {
		return itemErr(out, err.Error())
	}
	out["type"] = typeName(st)
	if st.IsDir() && !asBool(t, "recursive", true) {
		return itemErr(out, "source is a directory (set recursive=true)")
	}
	if asBool(t, "createDirs", true) {
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return itemErr(out, "create dir: "+err.Error())
		}
	}

	var toInfo os.FileInfo
	toExisted := false
	if fi, statErr := os.Lstat(to); statErr == nil {
		toInfo = fi
		toExisted = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return itemErr(out, "stat: "+statErr.Error())
	}
	overwrite := asBool(t, "overwrite", false)
	if toExisted && !overwrite {
		return itemErr(out, "target exists (overwrite=false)")
	}
	if toExisted && st.IsDir() != toInfo.IsDir() {
		return itemErr(out, "target type mismatch (source and target must both be file or both be dir)")
	}

	stats, cerr := copyPath(from, to, overwrite)
	if cerr != nil {
		if st.IsDir() && !toExisted {
			// 目标树完全由本次调用新建：整体回滚，恢复到调用前（不存在）的状态。
			if rerr := os.RemoveAll(to); rerr != nil {
				return itemErr(out, cerr.Error()+"; rollback failed: "+rerr.Error())
			}
		} else if copyFailurePartial(stats, toExisted, st, cerr) {
			// 目标原本存在：不删除用户已有数据，标记 partial。
			out["partial"] = true
			if st.IsDir() {
				out["copiedFiles"] = stats.files
			}
		}
		code, msg := copyFailureMessage(cerr, to)
		if code != "" {
			out["code"] = code
		}
		return itemErr(out, msg)
	}
	out["bytes"] = stats.bytes
	if st.IsDir() {
		out["files"] = stats.files
	}
	return itemOK(out)
}

// copyStats 累计复制结果（文件数 / 实际写入字节数 / 是否可能部分覆盖了已存在目标）。
type copyStats struct {
	files   int
	bytes   int64
	partial bool
}

// copyFailurePartial 判断复制失败是否需要标记 partial：
//   - 目标原本不存在 → 不回滚路径之外没有用户数据，不标 partial；
//   - 目标原本存在且复制已开始写（非 copyNoWriteError）→ partial；
//   - 目录合并到已存在目标 → 即使失败发生得较早也保守标记 partial（与既有语义一致）。
func copyFailurePartial(stats copyStats, toExisted bool, st os.FileInfo, err error) bool {
	if stats.partial {
		return true
	}
	if !toExisted {
		return false
	}
	if st != nil && st.IsDir() {
		return true
	}
	return !isCopyNoWrite(err)
}

// copyFailureMessage 把复制相关的稳定错误映射为返回文案与 code。
func copyFailureMessage(err error, to string) (code, msg string) {
	switch {
	case errors.Is(err, errFileInUse):
		return "FILE_IN_USE", "file is in use by another process: " + to
	case errors.Is(err, errTargetExists):
		return "", "target exists (overwrite=false)"
	case errors.Is(err, errIsDir):
		return "", "is a directory: " + to
	default:
		return "", err.Error()
	}
}

// copyNoWriteError 标记“失败发生在写入目标之前/目标未被改动”的复制错误
// （例如源打不开、目标独占打开失败、非 Windows 临时文件路径失败）。
type copyNoWriteError struct{ err error }

func (e *copyNoWriteError) Error() string { return e.err.Error() }
func (e *copyNoWriteError) Unwrap() error { return e.err }

// markCopyNoWrite 包装为“目标未被写入”的复制错误。
func markCopyNoWrite(err error) error {
	if err == nil {
		return nil
	}
	return &copyNoWriteError{err: err}
}

// isCopyNoWrite 判断复制错误是否确定发生在写入目标之前。
func isCopyNoWrite(err error) bool {
	var e *copyNoWriteError
	return errors.As(err, &e)
}

// copyPath 复制文件或目录（目录递归），返回已完成统计。
//
// 目录复制为合并语义：目标已存在时逐项覆盖同名文件，不删除目标中多出的文件。
// 中途失败返回已完成部分的 stats 与错误，由 copyOne/moveByCopy 决定回滚或标记 partial。
func copyPath(from, to string, overwrite bool) (copyStats, error) {
	st, err := os.Stat(from)
	if err != nil {
		return copyStats{}, markCopyNoWrite(err)
	}
	if st.IsDir() {
		return copyDir(from, to, overwrite)
	}
	n, err := copyFileAtomicFn(from, to, st, overwrite)
	if err != nil {
		return copyStats{partial: isPartialCopy(err)}, err
	}
	return copyStats{files: 1, bytes: n}, nil
}

// copyDir 递归复制目录（WalkDir 不跟随目录符号链接）。
func copyDir(from, to string, overwrite bool) (copyStats, error) {
	var stats copyStats
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(from, p)
		if rerr != nil {
			return rerr
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			// 新建目录沿用源目录 mode（Windows 不保留 ACL/属性，只尽力设置）。
			return ensureDir(dst, info.Mode().Perm())
		}
		// 文件符号链接按目标内容复制：Stat 跟随链接，mode/mtime 也取目标文件。
		info, ierr := os.Stat(p)
		if ierr != nil {
			return ierr
		}
		n, cerr := copyFileAtomicFn(p, dst, info, overwrite)
		if cerr != nil {
			stats.partial = stats.partial || isPartialCopy(cerr)
			return cerr
		}
		stats.files++
		stats.bytes += n
		return nil
	})
	return stats, err
}

// ensureDir 创建目录并（仅非 Windows）按源目录 mode 设置新建目录权限。
// 已存在的目录保持原权限（合并语义，不改动用户已有目录）。
//
// 目标树防护：用 Lstat 判定目标（不跟随链接）；目标是 symlink 时拒绝，
// 防止目录合并通过预先放置的子目录 junction/symlink 把写入重定向到白名单外。
func ensureDir(dst string, perm os.FileMode) error {
	fi, statErr := os.Lstat(dst)
	if statErr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("target path traverses symlink (refused)")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	existed := statErr == nil
	if err := os.MkdirAll(dst, perm); err != nil {
		return err
	}
	if existed || runtime.GOOS == "windows" {
		return nil
	}
	return os.Chmod(dst, perm)
}

// copyFileAtomicFn 是平台 copyFileAtomic 的测试替换点（注入复制失败点）。
var copyFileAtomicFn = copyFileAtomic

// partialCopyError 标记“已存在目标可能被部分覆盖”的复制失败。
// Windows 就地复制在开始写之后失败时返回它；非 Windows 的临时文件 + rename
// 不会改动目标，因此不会产生该错误。
type partialCopyError struct{ err error }

func (e *partialCopyError) Error() string { return e.err.Error() }
func (e *partialCopyError) Unwrap() error { return e.err }

// markCopyPartial 包装为“目标可能已部分更新”的复制错误。
func markCopyPartial(err error) error {
	if err == nil {
		return nil
	}
	return &partialCopyError{err: err}
}

// isPartialCopy 判断复制错误是否可能已部分覆盖目标。
func isPartialCopy(err error) bool {
	var e *partialCopyError
	return errors.As(err, &e)
}

// isWithin 判断 child 是否等于或位于 parent 之内。
func isWithin(child, parent string) bool {
	return safepath.IsWithin(child, parent)
}

// samePath 判断两个路径是否指向同一位置（不做磁盘解析）：
// Windows 大小写不敏感且分隔符归一；其他平台按 Clean 后精确比较。
func samePath(a, b string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.ToSlash(ca), filepath.ToSlash(cb))
	}
	return ca == cb
}

// typeName 返回类型名（file / dir）。
func typeName(st os.FileInfo) string {
	if st.IsDir() {
		return "dir"
	}
	return "file"
}
