// fsops.go — file_delete / file_move / file_copy。
package filetool

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
)

// runDelete 实现 file_delete。
func runDelete(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	for i, t := range req.targets {
		path, perr := resolvePath(asString(t, "path"), fmt.Sprintf("targets[%d].path", i))
		if perr != "" {
			results = append(results, itemErr(map[string]any{"path": asString(t, "path")}, perr))
			continue
		}
		results = append(results, deleteOne(path, t))
	}
	return finish(tool, req.purpose, results)
}

// deleteOne 删除单个文件或目录。
func deleteOne(path string, t map[string]any) map[string]any {
	out := map[string]any{"path": path}
	if reason := unsafeRoot(path); reason != "" {
		return itemErr(out, reason)
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) && asBool(t, "missingOk", false) {
			return itemSkip(out, "not found")
		}
		return itemErr(out, err.Error())
	}
	if st.IsDir() {
		out["type"] = "dir"
		if !asBool(t, "recursive", false) {
			entries, _ := os.ReadDir(path)
			if len(entries) > 0 {
				return itemErr(out, "directory is not empty (set recursive=true to delete)")
			}
		}
	} else {
		out["type"] = "file"
		out["size"] = st.Size()
		if sum, err := md5File(path); err == nil {
			out["md5"] = sum
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return itemErr(out, err.Error())
	}
	return itemOK(out)
}

// unsafeRoot 拒绝删除文件系统根 / 盘符根 / 用户主目录根。
func unsafeRoot(path string) string {
	clean := filepath.Clean(path)
	if filepath.Dir(clean) == clean {
		return "refuse to delete filesystem root"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if strings.EqualFold(clean, filepath.Clean(home)) {
			return "refuse to delete user home directory"
		}
	}
	// 盘符根（C:\）在 Clean 后形如 C:\，其 Dir 仍是自身 → 已被上面拦住；
	// 额外挡住 C:\Users 这类一级目录。
	if vol := filepath.VolumeName(clean); vol != "" {
		rest := strings.TrimPrefix(clean, vol)
		rest = strings.Trim(rest, `\/`)
		if rest == "" || strings.EqualFold(rest, "users") || strings.EqualFold(rest, "windows") {
			return "refuse to delete a system-level directory"
		}
	}
	return ""
}

// runMove 实现 file_move。
func runMove(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	for i, t := range req.targets {
		from, ferr := resolvePath(asString(t, "from"), fmt.Sprintf("targets[%d].from", i))
		to, terr := resolvePath(asString(t, "to"), fmt.Sprintf("targets[%d].to", i))
		if ferr != "" || terr != "" {
			results = append(results, itemErr(map[string]any{
				"from": asString(t, "from"), "to": asString(t, "to"),
			}, firstNonEmpty(ferr, terr)))
			continue
		}
		results = append(results, moveOne(from, to, t))
	}
	return finish(tool, req.purpose, results)
}

// moveOne 移动/重命名单个目标。
func moveOne(from, to string, t map[string]any) map[string]any {
	out := map[string]any{"from": from, "to": to}
	if isWithin(to, from) {
		return itemErr(out, "target is inside source (self-nested move)")
	}
	st, err := os.Stat(from)
	if err != nil {
		return itemErr(out, err.Error())
	}
	out["type"] = typeName(st)
	if _, err := os.Stat(to); err == nil && !asBool(t, "overwrite", false) {
		return itemErr(out, "target exists (overwrite=false)")
	}
	if asBool(t, "createDirs", true) {
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return itemErr(out, "create dir: "+err.Error())
		}
	}
	// 同卷 rename；跨卷回退为「复制成功后删源」。
	if err := os.Rename(from, to); err == nil {
		return itemOK(out)
	}
	if err := copyPath(from, to); err != nil {
		return itemErr(out, "cross-volume move failed at copy (source kept): "+err.Error())
	}
	if err := os.RemoveAll(from); err != nil {
		return itemErr(out, "copied but failed to remove source: "+err.Error())
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
	for i, t := range req.targets {
		from, ferr := resolvePath(asString(t, "from"), fmt.Sprintf("targets[%d].from", i))
		to, terr := resolvePath(asString(t, "to"), fmt.Sprintf("targets[%d].to", i))
		if ferr != "" || terr != "" {
			results = append(results, itemErr(map[string]any{
				"from": asString(t, "from"), "to": asString(t, "to"),
			}, firstNonEmpty(ferr, terr)))
			continue
		}
		results = append(results, copyOne(from, to, t))
	}
	return finish(tool, req.purpose, results)
}

// copyOne 复制单个目标。
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
	if _, err := os.Stat(to); err == nil && !asBool(t, "overwrite", false) {
		return itemErr(out, "target exists (overwrite=false)")
	}
	if asBool(t, "createDirs", true) {
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return itemErr(out, "create dir: "+err.Error())
		}
	}
	if err := copyPath(from, to); err != nil {
		return itemErr(out, err.Error())
	}
	if !st.IsDir() {
		out["bytes"] = st.Size()
	}
	return itemOK(out)
}

// copyPath 复制文件或目录（目录递归）。
func copyPath(from, to string) error {
	st, err := os.Stat(from)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, rerr := filepath.Rel(from, p)
			if rerr != nil {
				return rerr
			}
			dst := filepath.Join(to, rel)
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			return copyFile(p, dst)
		})
	}
	return copyFile(from, to)
}

// copyFile 复制单个文件（先写临时文件再 rename，避免半截目标）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	buf := make([]byte, 0, 64<<10)
	_ = buf
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	return writeAtomic(dst, data)
}

// isWithin 判断 child 是否等于或位于 parent 之内。
func isWithin(child, parent string) bool {
	c := strings.ToLower(filepath.Clean(child))
	p := strings.ToLower(filepath.Clean(parent))
	if c == p {
		return true
	}
	return strings.HasPrefix(c, p+string(os.PathSeparator))
}

// typeName 返回类型名（file / dir）。
func typeName(st os.FileInfo) string {
	if st.IsDir() {
		return "dir"
	}
	return "file"
}
