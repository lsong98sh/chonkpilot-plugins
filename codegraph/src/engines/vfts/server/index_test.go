// 文件级检索（Query）审计：相关度降序 / topK 截断 / 同文件多块去重 / path 子串过滤（大小写不敏感）。
//
// 期望值口径（不按实现内部排序函数反推）：
//   - docs/spec/20-modules/2B-vfts.md §4.3「Query：多取文档块（topK×5，上限 500）→ 文件级去重 →
//     取前 topK 个文件；path 子串过滤」；
//   - 同文档 §3 工具表「topK（默认 20，文件级去重）；path 子串过滤」。
//
// 由此可得：命中按**相关度**排序（同构文档下词频高者更相关 → 先返回）；文件级去重后条数**至多 topK**；
// path 过滤为**大小写不敏感的路径子串**匹配。
//
// 运行前置同 incremental_test.go 文件头（CGO + `zvec_c_api.dll` 在 PATH）。
package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noiseLines 与检索词无关的干扰文件内容：让查询词的 IDF 为正常量级（全库都含该词时 IDF 趋零，
// 分数被压平，无法判红词频差异）。
const noiseLines = "unrelated bananas and oranges\n"

// newIndexedWorkspace 建临时工作区 → 按 map 写文件（键为 '/' 相对路径）→ 全量索引。
// 返回 workspace 与索引结果（逐文件 Chunks 用于断言多块场景）。
func newIndexedWorkspace(t *testing.T, files map[string]string) (*Workspace, *IndexResult) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		writeText(t, dir, name, content)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { Drop(dir); CloseAll() })
	res, err := w.Initialize(nil, nil, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return w, res
}

// tfFiles 三个文档用**完全相同**的行文本，仅重复次数不同（词频 40 / 8 / 1）：
// 排除文档长度、词位置等其它变量，使相关度差异只来自词频。
func tfFiles() map[string]string {
	const line = "nectarine harvest line\n"
	return map[string]string{
		"hot.txt":    strings.Repeat(line, 40),
		"warm.txt":   strings.Repeat(line, 8),
		"cold.txt":   strings.Repeat(line, 1),
		"noise1.txt": noiseLines,
		"noise2.txt": noiseLines,
		"noise3.txt": noiseLines,
		"noise4.txt": noiseLines,
	}
}

// TestQueryRanksFilesByRelevanceDesc：3 个文件词频 40 > 8 > 1 → 命中文件按 Score 严格降序，
// 且顺序写死为 hot / warm / cold（口径 = 2B-vfts §4.3「按相关度」）。
func TestQueryRanksFilesByRelevanceDesc(t *testing.T) {
	w, _ := newIndexedWorkspace(t, tfFiles())

	hits, err := w.Query("nectarine", "", 10, "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	wantOrder := []string{"hot.txt", "warm.txt", "cold.txt"} // 词频 40 / 8 / 1
	if len(hits) != len(wantOrder) {
		t.Fatalf("应命中 %d 个文件：got=%d %+v", len(wantOrder), len(hits), hits)
	}
	for i, rel := range wantOrder {
		if exp := w.abs(rel); hits[i].Path != exp {
			t.Fatalf("第 %d 位应为词频更高的 %s：got=%s\nhits=%+v", i, exp, hits[i].Path, hits)
		}
	}
	for i := 0; i+1 < len(hits); i++ {
		if !(hits[i].Score > hits[i+1].Score) {
			t.Fatalf("命中未按 Score 降序：第 %d 位 score=%v 未大于第 %d 位 score=%v\nhits=%+v",
				i, hits[i].Score, i+1, hits[i+1].Score, hits)
		}
	}
	t.Logf("排序：hot=%v warm=%v cold=%v", hits[0].Score, hits[1].Score, hits[2].Score)
}

// TestQueryTopKTruncatesFiles：topK = 1 / 2 / 3 时文件数**精确**等于 topK（不超过、不缺省），
// 且为完整相关度序的前缀（口径 = 2B-vfts §3「topK（默认 20，文件级去重）」+ §4.3「取前 topK 个文件」）。
func TestQueryTopKTruncatesFiles(t *testing.T) {
	w, _ := newIndexedWorkspace(t, tfFiles())
	wantOrder := []string{"hot.txt", "warm.txt", "cold.txt"}

	for _, topK := range []int{1, 2, 3} {
		hits, err := w.Query("nectarine", "", topK, "")
		if err != nil {
			t.Fatalf("topK=%d query: %v", topK, err)
		}
		if len(hits) != topK {
			t.Fatalf("topK=%d 应精确返回 %d 个文件：got=%d %+v", topK, topK, len(hits), hits)
		}
		for i := 0; i < topK; i++ {
			if exp := w.abs(wantOrder[i]); hits[i].Path != exp {
				t.Fatalf("topK=%d 第 %d 位应为 %s：got=%s", topK, i, exp, hits[i].Path)
			}
		}
	}
}

// TestQueryDedupsChunksToFileLevel：multi.txt 被切成 3 块且每块都含检索词 → 文件级去重后只保留 1 条；
// 同时不得挤掉其它命中文件（去重后路径集合 = {multi.txt, a.txt, b.txt}，条数 = 3）。
// 口径 = 2B-vfts §4.3「文件级去重 → 取前 topK 个文件」。
func TestQueryDedupsChunksToFileLevel(t *testing.T) {
	files := map[string]string{
		// 240 行 × 1 词/行：按 80 行/块切分 → 3 块，每块都命中同一检索词。
		"multi.txt": strings.Repeat("nectarine multi chunk line\n", 240),
		"a.txt":     "nectarine only once\n",
		"b.txt":     "nectarine only once\n",
	}
	w, res := newIndexedWorkspace(t, files)

	byPath := map[string]int{}
	for _, f := range res.Indexed {
		byPath[f.Path] = f.Chunks
	}
	if byPath["multi.txt"] != 3 {
		t.Fatalf("前置：multi.txt 应切成 3 块（80 行/块），实际 %d（indexed=%+v）", byPath["multi.txt"], res.Indexed)
	}

	hits, err := w.Query("nectarine", "", 10, "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// 去重后条数 = 3 个不同文件（multi.txt 的 3 个块只计 1 条）。
	if len(hits) != 3 {
		t.Fatalf("文件级去重后应返回 3 条：got=%d %+v", len(hits), hits)
	}
	got := map[string]int{}
	for _, h := range hits {
		got[h.Path]++
	}
	want := []string{"multi.txt", "a.txt", "b.txt"}
	for _, rel := range want {
		p := w.abs(rel)
		if got[p] != 1 {
			t.Fatalf("%s 应恰出现 1 次：got=%d hits=%+v", p, got[p], hits)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("路径集合应恰为 %v：got=%v", want, got)
	}
}

// TestQueryPathFilterCaseInsensitiveSubstring：path 过滤 = 相对路径的**大小写不敏感子串**匹配。
// 正例（不同大小写、纯子串）命中；负例（不存在子串 / 不连续片段）为空。
// 口径 = 2B-vfts §3「path 子串过滤」+ §4.3「path 子串过滤」。
func TestQueryPathFilterCaseInsensitiveSubstring(t *testing.T) {
	// 目录名之间不得互为大小写变体（Windows 文件系统大小写不敏感，同名目录会合并、改写实际大小写）。
	files := map[string]string{
		"Alpha/Beta/Search.md":        "nectarine\n",
		"Other/Notes.md":              "nectarine\n",
		"delta/archive/search-old.md": "nectarine\n",
	}
	w, _ := newIndexedWorkspace(t, files)

	cases := []struct {
		name   string
		filter string
		want   []string // 期望的相对路径（空 = 无命中）
	}{
		{"小写子串", "alpha/beta", []string{"Alpha/Beta/Search.md"}},
		{"全大写命中混合大小写目录名", "ALPHA/BETA", []string{"Alpha/Beta/Search.md"}},
		{"混合大小写 + 纯子串", "aLpHa/BeT", []string{"Alpha/Beta/Search.md"}},
		{"文件名中段子串", "SEARCH-old", []string{"delta/archive/search-old.md"}},
		{"另一路径正例", "other", []string{"Other/Notes.md"}},
		{"负例：无该子串", "nomatch", nil},
		{"负例：片段不连续", "alpha/other", nil},
		{"负例：跨分隔符拼接", "beta-search", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits, err := w.Query("nectarine", "", 20, c.filter)
			if err != nil {
				t.Fatalf("query filter=%q: %v", c.filter, err)
			}
			if len(hits) != len(c.want) {
				t.Fatalf("filter=%q 命中数应为 %d：got=%d %+v", c.filter, len(c.want), len(hits), hits)
			}
			for i, rel := range c.want {
				if exp := w.abs(rel); hits[i].Path != exp {
					t.Fatalf("filter=%q 第 %d 位应为 %s：got=%s", c.filter, i, exp, hits[i].Path)
				}
			}
		})
	}
}
