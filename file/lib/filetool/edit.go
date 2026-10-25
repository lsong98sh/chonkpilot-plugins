// edit.go — file_replace / file_patch / file_diff。
package filetool

import (
	"fmt"
	"os"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/fileops"
)

// runReplace 实现 file_replace（精确文本替换，非正则）。
func runReplace(tool string, args map[string]any) *cli.Result {
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
		results = append(results, replaceOne(path, t))
	}
	return finish(tool, req.purpose, results)
}

// replaceOne 替换单个文件。
func replaceOne(path string, t map[string]any) map[string]any {
	find := asString(t, "find")
	if find == "" {
		return itemErr(map[string]any{"path": path}, "find is required and must be non-empty")
	}
	replace := asString(t, "replace")
	out := map[string]any{"path": path}

	oldData, encName, text, err := loadDecoded(path)
	if err != nil {
		return itemErr(out, err.Error())
	}
	before, _ := md5File(path)
	if want := strings.TrimSpace(asString(t, "expectMd5")); want != "" && !strings.EqualFold(want, before) {
		return itemSkip(map[string]any{"path": path, "md5": before}, "md5 mismatch")
	}

	count := strings.Count(text, find)
	if count == 0 {
		return itemErr(out, "search not found")
	}
	newText := text
	replaced := count
	if !asBool(t, "all", true) {
		newText = strings.Replace(text, find, replace, 1)
		replaced = 1
	} else {
		newText = strings.ReplaceAll(text, find, replace)
	}

	data, _, encErr := encodeWithEncoding(newText, encName, hasBOM(oldData))
	if encErr != nil {
		return itemErr(out, encErr.Error())
	}
	if err := writeAtomic(path, data); err != nil {
		return itemErr(out, "write: "+err.Error())
	}
	out["replacements"] = replaced
	out["md5Before"] = before
	if after, err := md5File(path); err == nil {
		out["md5After"] = after
	}
	if diff, err := fileops.RenderFileDiff(text, newText); err == nil {
		out["diff"] = diff
	}
	return itemOK(out)
}

// runPatch 实现 file_patch（unified diff）。
func runPatch(tool string, args map[string]any) *cli.Result {
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
		results = append(results, patchOne(path, t))
	}
	return finish(tool, req.purpose, results)
}

// patchOne 对单个文件应用补丁。
func patchOne(path string, t map[string]any) map[string]any {
	patch := asString(t, "patch")
	if strings.TrimSpace(patch) == "" {
		return itemErr(map[string]any{"path": path}, "patch is required")
	}
	out := map[string]any{"path": path}

	oldData, encName, text, err := loadDecoded(path)
	if err != nil {
		return itemErr(out, err.Error())
	}
	before, _ := md5File(path)
	if want := strings.TrimSpace(asString(t, "expectMd5")); want != "" && !strings.EqualFold(want, before) {
		return itemSkip(map[string]any{"path": path, "md5": before}, "md5 mismatch")
	}

	newText, applyErr := fileops.ApplyFileDiff(text, patch)
	if applyErr != nil {
		return itemErr(out, applyErr.Error())
	}
	data, _, encErr := encodeWithEncoding(newText, encName, hasBOM(oldData))
	if encErr != nil {
		return itemErr(out, encErr.Error())
	}
	if err := writeAtomic(path, data); err != nil {
		return itemErr(out, "write: "+err.Error())
	}
	out["hunks"] = strings.Count(patch, "\n@@")
	if strings.HasPrefix(patch, "@@") {
		out["hunks"] = out["hunks"].(int) + 1
	}
	out["md5Before"] = before
	if after, err := md5File(path); err == nil {
		out["md5After"] = after
	}
	return itemOK(out)
}

// runDiff 实现 file_diff（只读）。
func runDiff(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	for i, t := range req.targets {
		left, lerr := resolvePath(asString(t, "left"), fmt.Sprintf("targets[%d].left", i))
		right, rerr := resolvePath(asString(t, "right"), fmt.Sprintf("targets[%d].right", i))
		if lerr != "" || rerr != "" {
			results = append(results, itemErr(map[string]any{
				"left": asString(t, "left"), "right": asString(t, "right"),
			}, firstNonEmpty(lerr, rerr)))
			continue
		}
		results = append(results, diffOne(left, right))
	}
	return finish(tool, req.purpose, results)
}

// diffOne 比较一对文件。
func diffOne(left, right string) map[string]any {
	out := map[string]any{"left": left, "right": right}
	a, err := os.ReadFile(left)
	if err != nil {
		return itemErr(out, "left: "+err.Error())
	}
	b, err := os.ReadFile(right)
	if err != nil {
		return itemErr(out, "right: "+err.Error())
	}
	at, bt := fileops.DecodeText(a), fileops.DecodeText(b)
	if at == bt {
		out["identical"] = true
		out["added"] = 0
		out["removed"] = 0
		out["diff"] = ""
		return itemOK(out)
	}
	diff, derr := fileops.RenderFileDiff(at, bt)
	if derr != nil {
		return itemErr(out, derr.Error())
	}
	added, removed := countDiffLines(diff)
	out["identical"] = false
	out["added"] = added
	out["removed"] = removed
	out["diff"] = diff
	return itemOK(out)
}

// countDiffLines 统计 diff 文本中的新增/删除行（跳过 --- / +++ 文件头）。
func countDiffLines(diff string) (added, removed int) {
	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "+++"), strings.HasPrefix(ln, "---"):
			continue
		case strings.HasPrefix(ln, "+"):
			added++
		case strings.HasPrefix(ln, "-"):
			removed++
		}
	}
	return added, removed
}

// loadDecoded 读取文件并解码，同时给出用于回写的编码名。
func loadDecoded(path string) (oldData []byte, encName string, text string, err error) {
	st, statErr := os.Stat(path)
	if statErr != nil {
		return nil, "", "", statErr
	}
	if st.IsDir() {
		return nil, "", "", fmt.Errorf("is a directory")
	}
	oldData, err = os.ReadFile(path)
	if err != nil {
		return nil, "", "", err
	}
	encName = autoEncodeNameOf(oldData)
	text = fileops.DecodeText(oldData)
	return oldData, encName, text, nil
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
