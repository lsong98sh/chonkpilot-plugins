// edit.go — file_replace / file_patch / file_diff。
package filetool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/fileops"
	"github.com/lsong98sh/chonkpilot-util/encodingx"
	"github.com/lsong98sh/chonkpilot-util/strx"
)

// runReplace 实现 file_replace（精确文本替换，非正则）。
//
// 同路径合并（2026-11-09）：同一文件在批内的多个 target 只**独占打开一次、读一次、
// 写一次**；组内按 targets[] 输入顺序依次替换。文件级全有全无：任何一步失败都不写，
// 失败项 failed、同组其他项 skipped（not_applied）。锁调度见 batchlock.go。
func runReplace(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, len(req.targets))
	groups, order := groupEditTargets(req.targets, results)
	jobs := make([]batchJob, 0, len(order))
	for _, path := range order {
		jobs = append(jobs, newEditJob(path, groups[path], results,
			validateReplaceTarget, applyReplace, "replacements", "", false))
	}
	runBatchJobs(results, jobs)
	return finish(tool, results)
}

// runPatch 实现 file_patch（unified diff）。
//
// 同路径合并：同一文件多个 target 只独占打开/读/写一次，按 targets[] 顺序依次应用
// 每个 unified diff；文件级全有全无（任一 patch 应用失败 → 该文件不写）。
func runPatch(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	defaultEnc := normalizeEncoding(asString(args, "defaultEncoding"))
	if defaultEnc == "auto" {
		defaultEnc = ""
	}
	if defaultEnc != "" && !supportedEncoding(defaultEnc) {
		return paramErr(tool, "unsupported defaultEncoding: "+defaultEnc+"（配置页默认字符集无效）")
	}
	results := make([]map[string]any, len(req.targets))
	groups, order := groupEditTargets(req.targets, results)
	jobs := make([]batchJob, 0, len(order))
	for _, path := range order {
		jobs = append(jobs, newEditJob(path, groups[path], results,
			validatePatchTarget, applyPatch, "hunks", defaultEnc, true))
	}
	runBatchJobs(results, jobs)
	return finish(tool, results)
}

// editOp 是同一文件分组内的一个 target；index 为 results 下标，path 为解析后的路径。
type editOp struct {
	index  int
	path   string
	target map[string]any
}

// groupEditTargets 解析全部路径并按**解析后的路径**分组。
// 路径无效的 target 直接填 failed；有效 target 按入参顺序进入同路径组，
// order 是各路径首次出现的顺序（组执行顺序）。
func groupEditTargets(targets []map[string]any, results []map[string]any) (map[string][]editOp, []string) {
	groups := make(map[string][]editOp, len(targets))
	order := make([]string, 0, len(targets))
	for i, t := range targets {
		path, perr := resolvePath(asString(t, "path"), fmt.Sprintf("targets[%d].path", i))
		if perr != "" {
			results[i] = itemErr(map[string]any{"path": asString(t, "path")}, perr)
			continue
		}
		if _, ok := groups[path]; !ok {
			order = append(order, path)
		}
		groups[path] = append(groups[path], editOp{index: i, path: path, target: t})
	}
	return groups, order
}

// newEditJob 构造 replace/patch 的单个同路径分组调度单元。
// try 只在独占打开遇到 errFileInUse 时返回 true（不填结果，等待调度器重试）。
func newEditJob(path string, ops []editOp, results []map[string]any,
	validate func(map[string]any) string,
	apply func(text string, t map[string]any) (newText string, count int, errMsg string),
	countKey, defaultEnc string, allowDefaultEncoding bool) batchJob {
	indices := make([]int, len(ops))
	for i, op := range ops {
		indices[i] = op.index
	}
	return batchJob{
		indices: indices,
		try: func() bool {
			return runEditGroup(path, ops, results, validate, apply, countKey, defaultEnc, allowDefaultEncoding)
		},
		fillInUse: func() {
			editGroupFail(ops, results, "FILE_IN_USE",
				"file is in use by another process: "+path, false)
		},
	}
}

// runEditGroup 对同一文件执行一次「独占打开 → 读 → decode → expectMd5 → 按序应用 → 编码 → 写」。
//
// 返回 inUse=true 表示本次未拿到独占句柄（errFileInUse），未填任何结果，等待批调度重试；
// 其余情况（成功/fatal 失败）按文件级全有全无填好同组所有结果并返回 false。
//
// 失败语义：
//   - 组内参数无效 / 第 k 步应用失败 → 第 k 项 failed，同组其他项 skipped（not_applied），不写；
//   - 同组多个不同的非空 expectMd5 → 全部 failed（code=conflicting_expectMd5）；
//   - expectMd5 不符 → 全部 skipped（含当前 md5）；
//   - 打开/读/解码/编码失败 → 全部 failed；
//   - 写失败 → 全部 failed；用 oldData 回滚，回滚失败附 partial:true。
func runEditGroup(path string, ops []editOp, results []map[string]any,
	validate func(map[string]any) string,
	apply func(text string, t map[string]any) (newText string, count int, errMsg string),
	countKey, defaultEnc string, allowDefaultEncoding bool) bool {
	// 1. 组内参数校验：第一项无效 → 该项 failed、其余 skipped，不打开文件。
	for k, op := range ops {
		if msg := validate(op.target); msg != "" {
			editGroupStepFailure(ops, results, k, msg)
			return false
		}
	}

	// 2. expectMd5：相同值只校验一次；多个不同非空值属于调用方矛盾 → 全部 failed。
	want, conflict := editGroupExpectMD5(ops)
	if conflict {
		for _, op := range ops {
			results[op.index] = itemErr(map[string]any{
				"path": op.path, "code": "conflicting_expectMd5", "reason": "conflicting_expectMd5",
			}, "conflicting_expectMd5")
		}
		return false
	}

	// 3. 一次独占打开（单次尝试；共享/锁冲突交给调度器记 pending）。
	f, err := openExclusiveExistingOnce(path)
	if err != nil {
		if errors.Is(err, errFileInUse) {
			return true
		}
		editGroupFail(ops, results, "", err.Error(), false)
		return false
	}
	defer f.Close()

	oldData, rerr := io.ReadAll(f)
	if rerr != nil {
		editGroupFail(ops, results, "", "read: "+rerr.Error(), false)
		return false
	}
	srcDefault := ""
	if allowDefaultEncoding {
		srcDefault = defaultEnc
	}
	encName, text, derr := decodeSource(oldData, srcDefault)
	if derr != nil {
		editGroupFail(ops, results, "", derr.Error(), false)
		return false
	}
	if want != "" {
		before := md5Hex(oldData)
		if !strings.EqualFold(want, before) {
			for _, op := range ops {
				results[op.index] = itemSkip(map[string]any{"path": op.path, "md5": before}, "md5 mismatch")
			}
			return false
		}
	}

	// 4. 按 targets[] 输入顺序在内存中依次应用；任一步失败 → 不写。
	newText := text
	counts := make([]int, len(ops))
	for k, op := range ops {
		next, count, emsg := apply(newText, op.target)
		if emsg != "" {
			editGroupStepFailure(ops, results, k, emsg)
			return false
		}
		newText = next
		counts[k] = count
	}

	// 5. 一次编码、一次写。
	// patch：源文件是 Windows 中文 ANSI（GBK/GB18030）且配置了默认字符集时，转为默认字符集。
	// replace：保持原编码（require 只要求 patch）。
	outEnc := encName
	if allowDefaultEncoding && defaultEnc != "" && isANSIEncoding(encName) {
		outEnc = defaultEnc
	}
	data, _, encErr := encodeWithEncoding(newText, outEnc, hasBOM(oldData))
	if encErr != nil {
		editGroupFail(ops, results, "", encErr.Error(), false)
		return false
	}
	if _, werr := writeInPlace(f, data); werr != nil {
		editGroupWriteFailure(ops, results, f, oldData, werr)
		return false
	}

	// 6. 成功：每项 ok + 自己的 replacements/hunks。
	for k, op := range ops {
		results[op.index] = itemOK(map[string]any{"path": op.path, "encoding": outEnc, countKey: counts[k]})
	}
	return false
}

// editGroupExpectMD5 返回组内唯一的非空 expectMd5（去空白）。
// 出现两个不同的非空值（大小写不敏感比较）→ conflict=true。
func editGroupExpectMD5(ops []editOp) (string, bool) {
	want := ""
	for _, op := range ops {
		v := strings.TrimSpace(asString(op.target, "expectMd5"))
		if v == "" {
			continue
		}
		if want == "" {
			want = v
			continue
		}
		if !strings.EqualFold(want, v) {
			return "", true
		}
	}
	return want, false
}

// validateReplaceTarget / applyReplace 是 file_replace 的分组校验与单步应用。
func validateReplaceTarget(t map[string]any) string {
	if asString(t, "find") == "" {
		return "find is required and must be non-empty"
	}
	return ""
}

func applyReplace(text string, t map[string]any) (string, int, string) {
	find := asString(t, "find")
	count := strings.Count(text, find)
	if count == 0 {
		return "", 0, "search not found"
	}
	if !asBool(t, "all", true) {
		return strings.Replace(text, find, asString(t, "replace"), 1), 1, ""
	}
	return strings.ReplaceAll(text, find, asString(t, "replace")), count, ""
}

// validatePatchTarget / applyPatch 是 file_patch 的分组校验与单步应用。
func validatePatchTarget(t map[string]any) string {
	if strings.TrimSpace(asString(t, "patch")) == "" {
		return "patch is required"
	}
	return ""
}

func applyPatch(text string, t map[string]any) (string, int, string) {
	patch := asString(t, "patch")
	next, err := fileops.ApplyFileDiff(text, patch)
	if err != nil {
		return "", 0, err.Error()
	}
	return next, countHunks(patch), ""
}

// editGroupStepFailure：第 k 步失败 → 该项 failed，同组其他项 skipped（not_applied）。
func editGroupStepFailure(ops []editOp, results []map[string]any, k int, msg string) {
	for j, op := range ops {
		if j == k {
			results[op.index] = itemErr(map[string]any{"path": op.path}, msg)
			continue
		}
		results[op.index] = itemSkip(map[string]any{"path": op.path}, "not_applied")
	}
}

// editGroupFail：同组所有项填同一 failed（code 为空时省略；partial 仅在回滚失败时置 true）。
func editGroupFail(ops []editOp, results []map[string]any, code, msg string, partial bool) {
	for _, op := range ops {
		out := map[string]any{"path": op.path}
		if code != "" {
			out["code"] = code
		}
		if partial {
			out["partial"] = true
		}
		results[op.index] = itemErr(out, msg)
	}
}

// editGroupWriteFailure 处理 replace/patch 的就地写失败：用 oldData 回滚
// （Seek 0、写回、Truncate、Sync），同组所有项 failed。
// 回滚失败 → 所有项 partial=true 并明确报错；回滚成功 → 普通 failed（文件保持原状）。
func editGroupWriteFailure(ops []editOp, results []map[string]any, f *os.File, oldData []byte, werr error) {
	msg := "write: " + werr.Error()
	if _, rberr := writeInPlace(f, oldData); rberr != nil {
		msg += "; rollback failed, target may be incomplete: " + rberr.Error()
		editGroupFail(ops, results, "", msg, true)
		return
	}
	msg += " (rolled back)"
	editGroupFail(ops, results, "", msg, false)
}

// countHunks 统计补丁中的 @@ 头数量（内容行均带 +/-/空格前缀，不会误计）。
func countHunks(patch string) int {
	n := 0
	for _, ln := range strings.Split(patch, "\n") {
		if strings.HasPrefix(strings.TrimSuffix(ln, "\r"), "@@") {
			n++
		}
	}
	return n
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
		context := asInt(t, "context", fileops.DefaultDiffContext)
		if context < 0 {
			context = 0
		}
		results = append(results, diffOne(left, right, context))
	}
	return finish(tool, results)
}

// maxDiffReturnBytes 是 file_diff 单次返回的 diff 上限（128K，按行边界截断）。
// 输入不再按大小拒绝；超出部分用 diff_truncated/diff_omitted 标识“未返回”。
const maxDiffReturnBytes = 128 << 10

// diffOne 比较一对文件（文本级：按各自源编码解码后比较）。
//
// 返回字段：
//   - identical = 解码后的文本内容一致；
//   - left_encoding/right_encoding/left_bom/right_bom = 两侧编码与 BOM（内容一致但编码不同时仍给出）；
//   - diff 最多 128K；超出时 diff_truncated/diff_omitted=true，diff_bytes 为完整 diff 字节数。
//
// 任一侧头部被判定为二进制（含 NUL）→ 该项 failed，code=BINARY_UNSUPPORTED、binary=true，
// 不做解码/比较/diff。
func diffOne(left, right string, context int) map[string]any {
	out := map[string]any{"left": left, "right": right}
	a, err := os.ReadFile(left)
	if err != nil {
		return itemErr(out, "left: "+err.Error())
	}
	b, err := os.ReadFile(right)
	if err != nil {
		return itemErr(out, "right: "+err.Error())
	}
	leftHead := a
	if len(leftHead) > encodingProbeBytes {
		leftHead = leftHead[:encodingProbeBytes]
	}
	if detectEncoding(leftHead).Binary {
		out["code"] = "BINARY_UNSUPPORTED"
		out["binary"] = true
		return itemErr(out, "left: binary file (not supported)")
	}
	rightHead := b
	if len(rightHead) > encodingProbeBytes {
		rightHead = rightHead[:encodingProbeBytes]
	}
	if detectEncoding(rightHead).Binary {
		out["code"] = "BINARY_UNSUPPORTED"
		out["binary"] = true
		return itemErr(out, "right: binary file (not supported)")
	}
	leftEnc := encodingx.AutoName(a)
	if leftEnc == "" {
		leftEnc = encodingx.UTF8
	}
	rightEnc := encodingx.AutoName(b)
	if rightEnc == "" {
		rightEnc = encodingx.UTF8
	}
	out["left_encoding"] = leftEnc
	out["right_encoding"] = rightEnc
	out["left_bom"] = hasBOM(a)
	out["right_bom"] = hasBOM(b)

	at, aerr := decodeWithEncoding(a, leftEnc)
	if aerr != nil {
		return itemErr(out, "left decode as "+leftEnc+": "+aerr.Error())
	}
	bt, berr := decodeWithEncoding(b, rightEnc)
	if berr != nil {
		return itemErr(out, "right decode as "+rightEnc+": "+berr.Error())
	}
	if at == bt {
		out["identical"] = true
		out["added"] = 0
		out["removed"] = 0
		out["diff"] = ""
		return itemOK(out)
	}
	diff, derr := fileops.RenderFileDiff(at, bt,
		filepath.ToSlash(left), filepath.ToSlash(right), context)
	if derr != nil {
		return itemErr(out, derr.Error())
	}
	added, removed := countDiffLines(diff)
	out["identical"] = false
	out["added"] = added
	out["removed"] = removed
	total := len(diff)
	if total > maxDiffReturnBytes {
		shown := truncateDiffAtLine(diff, maxDiffReturnBytes)
		out["diff"] = shown
		out["diff_truncated"] = true
		out["diff_omitted"] = true
		out["diff_bytes"] = total
		out["diff_returned_bytes"] = len(shown)
	} else {
		out["diff"] = diff
	}
	return itemOK(out)
}

// truncateDiffAtLine 把 diff 截到 limit 字节以内，尽量停在完整行边界并保证 UTF-8 合法。
func truncateDiffAtLine(diff string, limit int) string {
	if limit <= 0 || len(diff) <= limit {
		return diff
	}
	cut := diff[:limit]
	if i := strings.LastIndexByte(cut, '\n'); i >= 0 {
		cut = cut[:i+1]
	}
	return strings.ToValidUTF8(cut, "")
}

// countDiffLines 统计 diff 文本中的新增/删除行（只统计 @@ 头之后的行；含 `++`/`--` 内容）。
func countDiffLines(diff string) (added, removed int) {
	inHunk := false
	for _, ln := range strings.Split(diff, "\n") {
		if !inHunk {
			if strings.HasPrefix(ln, "@@") {
				inHunk = true
			}
			continue
		}
		switch {
		case strings.HasPrefix(ln, `\`):
			continue
		case strings.HasPrefix(ln, "+"):
			added++
		case strings.HasPrefix(ln, "-"):
			removed++
		}
	}
	return added, removed
}

// decodeSource 对已读入内存的内容做二进制拒绝与“同一编码名”解码。
//
// 编码口径（replace/patch）：**解码与回写必须使用同一个编码名**。
// 用 `encodingx.AutoName` 取编码名（其识别顺序与 `DecodeAuto` 完全一致），再用
// `decodeWithEncoding` 按该名解码；不能先 `DecodeText`（独立探测）再另取名回写，
// 否则可能出现“按 GBK 解码、按 GB18030/Big5 回写”的跨编码损坏。
//
// 二进制文件拒绝；UTF-8 BOM 在解码时剥掉，回写时由 `hasBOM(oldData)` 决定是否加回。
func decodeSource(oldData []byte, defaultEnc string) (encName string, text string, err error) {
	head := oldData
	if len(head) > encodingProbeBytes {
		head = head[:encodingProbeBytes]
	}
	if det := detectEncoding(head); det.Binary {
		return "", "", errors.New("binary file (not supported)")
	}
	name, emsg := autoEncodeNameStrict(head)
	if emsg != "" {
		// 短样本/低置信度：配置了默认字符集且数据像 GBK 中文 → 按 ANSI 处理；
		// 否则沿用旧兜底（UTF-8），保持 replace/patch 既有行为。
		if defaultEnc != "" && looksLikeANSI(head) {
			name = encodingx.GBK
		} else {
			name = encodingx.AutoName(oldData)
		}
	}
	encName = normalizeEncoding(name)
	if encName == "" || encName == "auto" {
		encName = encodingx.UTF8
	}
	text, derr := decodeWithEncoding(oldData, encName)
	if derr != nil {
		return "", "", fmt.Errorf("decode as %s: %w", encName, derr)
	}
	return encName, text, nil
}

// decodeWithEncoding 按**已确定的编码名**解码，保证 replace/patch 的“读→改→写”
// 全程同一编码。UTF-8 先剥 BOM（回写时按旧文件 BOM 状态补回）；UTF-16 由 encodingx.Decode
// 去 BOM；legacy 编码无 BOM。
func decodeWithEncoding(data []byte, encName string) (string, error) {
	if encName == encodingx.UTF8 {
		return string(stripUTF8BOM(data)), nil
	}
	return encodingx.Decode(encName, data)
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	return strx.FirstNonEmpty(vals...)
}
