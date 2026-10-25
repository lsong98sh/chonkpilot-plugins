// 增量索引冒烟（真实执行 zvec C-API）：建 3 个文件 → 全量索引 → 改 1 个文件 → 再次增量 →
// 断言只重建 1 个文件（其余跳过）、被删文件块被移除、旧内容检索不到。
//
// 运行需 CGO 环境 + `zvec_c_api.dll` 在 PATH（唯一入口 = docs/spec/50-testing/50-测试体系.md §5.1 的可复制命令）：
//
//	$env:CGO_ENABLED="1"; $env:CGO_CFLAGS="-I<...>/third_party/zvec/include"
//	$env:CGO_LDFLAGS="-L<...>/third_party/zvec/windows_amd64 -lzvec_c_api"
//	PATH += <...>/third_party/zvec/windows_amd64（zvec_c_api.dll 运行期加载；缺失则 exit status 0xc0000135）
package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-plugins/codebase/engines/vfts/third_party/jieba"
)

func writeText(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestCollectFilesStackGitignore：stack_gitignore 关 → 不读 .gitignore（仅内置强制 + 默认 + 用户规则）；
// 开 → 按 gitignore 语义过滤（目录不下降、文件级排除、'!' 反选）。
func TestCollectFilesStackGitignore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // 隔离全局 ignore

	cases := []struct {
		name  string
		gitig string
		stack bool
		files []string
	}{
		{"stack 关：不读 .gitignore", "generated/\na.txt\n", false,
			[]string{"a.txt", "generated/gen.txt", "keep.txt", "sub/b.txt"}},
		{"stack 开：目录不下降 + 文件级排除", "generated/\na.txt\n", true,
			[]string{"keep.txt", "sub/b.txt"}},
		{"stack 开：'!' 反选", "*.txt\n!keep.txt\n", true,
			[]string{"keep.txt"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range []string{"a.txt", "keep.txt", "generated/gen.txt", "sub/b.txt"} {
				p := filepath.Join(dir, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(c.gitig), 0o644); err != nil {
				t.Fatal(err)
			}

			w, err := Open(dir)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { Drop(dir); CloseAll() }()

			stack := c.stack
			if err := w.Configure(nil, []string{".txt"}, nil, &stack, nil); err != nil {
				t.Fatalf("configure: %v", err)
			}
			entries, _, err := w.collectFiles()
			if err != nil {
				t.Fatalf("collectFiles: %v", err)
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.path)
			}
			if strings.Join(got, ",") != strings.Join(c.files, ",") {
				t.Fatalf("文件清单不符：got=%v want=%v", got, c.files)
			}
		})
	}
}

func TestIncrementalIndexSmoke(t *testing.T) {
	dir := t.TempDir()
	writeText(t, dir, "a.txt", "alpha apple\n")
	writeText(t, dir, "b.txt", "bravo berry\n")
	writeText(t, dir, "c.txt", "charlie cherry\n")

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { Drop(dir); CloseAll() }()

	// 1) 全量首建
	full, err := w.Initialize(nil, nil, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Logf("首建：mode=%s added=%d chunks=%d indexed=%d", full.Mode, full.Added, full.Chunks, len(full.Indexed))
	if full.Mode != "full" || full.Added != 3 {
		t.Fatalf("首建应全量索引 3 文件：%+v", full)
	}
	byPath := map[string]IndexedFile{}
	for _, f := range full.Indexed {
		byPath[f.Path] = f
		if len(f.DocIDs) == 0 {
			t.Fatalf("%s 应有 doc_ids：%+v", f.Path, f)
		}
	}
	aLive, cLive := byPath["a.txt"], byPath["c.txt"]
	if len(aLive.DocIDs) == 0 || len(cLive.DocIDs) == 0 {
		t.Fatalf("a/c 应有块：a=%+v c=%+v", aLive, cLive)
	}

	// 2) 只改 a.txt → 增量：updated=1、其余跳过（未出现在 files 即不动）
	writeText(t, dir, "a.txt", "beta banana\n")
	inc, err := w.Incremental([]IncrementalFile{{
		Path: filepath.Join(dir, "a.txt"), Key: "k-a", DocIDs: aLive.DocIDs,
	}}, nil)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	t.Logf("改 a.txt 增量：mode=%s added=%d updated=%d removed=%d removedChunks=%d newChunks=%d",
		inc.Mode, inc.Added, inc.Updated, inc.Removed, inc.RemovedChunks, inc.Chunks)
	if inc.Mode != "incremental" || inc.Updated != 1 || inc.Added != 0 {
		t.Fatalf("应只更新 1 个文件：%+v", inc)
	}
	if inc.RemovedChunks != len(aLive.DocIDs) {
		t.Fatalf("应删除 a.txt 旧块 %d 个，实为 %d", len(aLive.DocIDs), inc.RemovedChunks)
	}
	if len(inc.Indexed) != 1 || inc.Indexed[0].Path != "a.txt" {
		t.Fatalf("增量结果应仅含 a.txt：%+v", inc.Indexed)
	}
	for _, id := range aLive.DocIDs {
		for _, nid := range inc.Indexed[0].DocIDs {
			if id == nid {
				t.Fatalf("主键复用：%s", id)
			}
		}
	}

	// 3) 被改文件：旧内容检索不到、新内容命中；未动的 b.txt 仍可命中
	if hits, err := w.Query("alpha", "", 20, ""); err != nil || len(hits) != 0 {
		t.Fatalf("a.txt 旧内容应已移除：hits=%d err=%v", len(hits), err)
	}
	if hits, err := w.Query("banana", "", 20, ""); err != nil || len(hits) != 1 ||
		hits[0].Path != filepath.ToSlash(filepath.Join(dir, "a.txt")) {
		t.Fatalf("a.txt 新内容应命中 1 处：%+v err=%v", hits, err)
	}
	if hits, err := w.Query("berry", "", 20, ""); err != nil || len(hits) != 1 {
		t.Fatalf("b.txt 应保持命中：%+v err=%v", hits, err)
	}

	// 4) 删除 c.txt → 增量仅按 doc_ids 删块
	if err := os.Remove(filepath.Join(dir, "c.txt")); err != nil {
		t.Fatal(err)
	}
	rem, err := w.Incremental(nil, []IncrementalRemove{{Key: "k-c", DocIDs: cLive.DocIDs}})
	if err != nil {
		t.Fatalf("incremental remove: %v", err)
	}
	t.Logf("删 c.txt 增量：mode=%s removed=%d removedChunks=%d newChunks=%d",
		rem.Mode, rem.Removed, rem.RemovedChunks, rem.Chunks)
	if rem.Removed != 1 || rem.RemovedChunks != len(cLive.DocIDs) {
		t.Fatalf("应移除 c.txt 的 %d 块：%+v", len(cLive.DocIDs), rem)
	}
	if hits, err := w.Query("charlie", "", 20, ""); err != nil || len(hits) != 0 {
		t.Fatalf("c.txt 内容应已移除：hits=%d err=%v", len(hits), err)
	}
	t.Logf("状态：files=%d chunks=%d state=%s", w.Status().IndexedFiles, w.Status().ChunkCount, w.Status().State)
}

// TestTokenizerSemanticsJieba：锁定 zvec `jieba` 分词器（+ lowercase 过滤器）的可判红语义
// （对齐 docs/spec/20-modules/2B-vfts.md §4.1）：
//
//	① 中文按**词**切分 → 词典词命中（"检索"/"全文"）；跨词相邻字串（"文检"）**不**命中——
//	   standard 分词器下会命中，故此断言是 **jieba 生效的判红点**；
//	② 英文大小写归一（lowercase 过滤器）：小写 keyword 命中索引里的大写 Keyword；
//	③ 无词干化：run 不命中 running。
//
// 运行前置同本文件头（CGO + `zvec_c_api.dll` 在 PATH）。
func TestTokenizerSemanticsJieba(t *testing.T) {
	dir := t.TempDir()
	writeText(t, dir, "cn.txt", "全文检索\n")
	writeText(t, dir, "en.txt", "Keyword running\n")

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { Drop(dir); CloseAll() }()

	res, err := w.Initialize(nil, nil, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.Added != 2 {
		t.Fatalf("应索引 2 文件：%+v", res)
	}

	// ① 词级命中：词典词（完整词/前缀词）命中。
	for _, q := range []string{"检索", "全文"} {
		hits, err := w.Query(q, "", 20, "")
		if err != nil || len(hits) != 1 {
			t.Fatalf("中文词 %q 应命中 1 处：hits=%d err=%v", q, len(hits), err)
		}
	}
	// ①b 跨词相邻字串（非词）不命中：jieba 生效判红点（standard 下会按单字命中）。
	if hits, err := w.Query("文检", "", 20, ""); err != nil || len(hits) != 0 {
		t.Fatalf("jieba 下非词字串 \"文检\" 不应命中（standard 会命中）：hits=%d err=%v", len(hits), err)
	}
	// ② 英文大小写归一：小写 keyword 命中索引里的大写 Keyword。
	if hits, err := w.Query("keyword", "", 20, ""); err != nil || len(hits) != 1 {
		t.Fatalf("小写应命中大写 Keyword：hits=%d err=%v", len(hits), err)
	}
	// ③ 无词干化：run 不命中 running。
	if hits, err := w.Query("run", "", 20, ""); err != nil || len(hits) != 0 {
		t.Fatalf("run 不应命中 running（无词干化）：hits=%d err=%v", len(hits), err)
	}
}

// TestSystemDictMaterializeAndTools：系统级 jieba 词典物化 + 查看/编辑工具往返。
// ① ensureSystemDict 落盘基础词典（大小与内嵌一致）与 user_dict.txt；
// ② vfts_dict_get 返回目录 / 基础词典名 / 自定义词全文 / 词条数；
// ③ vfts_dict_set 写入后可读回，词条数按「非空非注释行」统计。
func TestSystemDictMaterializeAndTools(t *testing.T) {
	dir, err := ensureSystemDict()
	if err != nil {
		t.Fatalf("ensureSystemDict: %v", err)
	}
	if dir != SystemDictDir() {
		t.Fatalf("目录不一致：%s vs %s", dir, SystemDictDir())
	}
	for _, name := range []string{jieba.DictName, jieba.HMMName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("基础词典 %s 未物化：%v", name, err)
		}
		raw, err := jieba.Read(name)
		if err != nil {
			t.Fatalf("内嵌词典 %s 读取失败：%v", name, err)
		}
		if fi.Size() != int64(len(raw)) {
			t.Fatalf("基础词典 %s 大小不符：%d vs %d", name, fi.Size(), len(raw))
		}
	}

	// 自定义词写入 → 往返（用 t.Cleanup 恢复原值，避免污染机器上的系统级词典）。
	orig, err := ReadUserDict()
	if err != nil {
		t.Fatalf("ReadUserDict: %v", err)
	}
	t.Cleanup(func() { _ = WriteUserDict(orig) })

	got, err := toolDictGet(context.Background(), nil)
	if err != nil {
		t.Fatalf("vfts_dict_get: %v", err)
	}
	gm := got.(map[string]any)
	if gm["dict_dir"] != dir || gm["user_dict_path"] != UserDictPath() {
		t.Fatalf("dict_get 路径不符：%+v", gm)
	}
	if base, _ := gm["base_dicts"].([]string); len(base) != 2 {
		t.Fatalf("base_dicts 应含 2 项：%+v", gm["base_dicts"])
	}

	if _, err := toolDictSet(context.Background(), map[string]any{"user_dict": "# 注释\n朝彻\nChonkPilot\n\n"}); err != nil {
		t.Fatalf("vfts_dict_set: %v", err)
	}
	ud, err := ReadUserDict()
	if err != nil {
		t.Fatalf("ReadUserDict: %v", err)
	}
	if !strings.Contains(ud, "朝彻") || !strings.Contains(ud, "ChonkPilot") {
		t.Fatalf("自定义词未落盘：%q", ud)
	}
	got2, err := toolDictGet(context.Background(), nil)
	if err != nil {
		t.Fatalf("vfts_dict_get #2: %v", err)
	}
	if wc := got2.(map[string]any)["word_count"]; wc != 2 {
		t.Fatalf("词条数应为 2（注释/空行不计）：got=%v", wc)
	}
}
