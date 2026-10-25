// dict.go：系统级 jieba 词典（「只有系统级」——全机/全工作区共用一份，
// 不做项目级 / 用户级覆盖；与 2B-vfts 的词典口径一致）。
//
// 目录 = <系统缓存根>/chonkpilot/vfts/jieba（Windows = %LocalAppData%\chonkpilot\vfts\jieba）：
//   - jieba.dict.utf8 / hmm_model.utf8：**内嵌**基础词典（首次使用物化，只读不改；随引擎版本更新）
//   - user_dict.txt：系统级**自定义词**（每行一词，`#` 注释行忽略）→ 叠加在基础词典之上
//
// 「查看 / 编辑」入口经引擎工具 vfts_dict_get / vfts_dict_set（管理工具，不发给 LLM）。
// 词典（含自定义词）变更后**必须重建索引**才生效（分词结果落在索引里）。
package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lsong98sh/chonkpilot-plugins/codebase/engines/vfts/third_party/jieba"
)

// userDictName 系统级自定义词文件名（zvec jieba `user_dict_path`）。
const userDictName = "user_dict.txt"

// systemDictOnce 单飞解析 systemDictDir。词典目录路径仅由本 Once 写入（只读消费），
// 消除「SystemDictDir 读 / ensureSystemDict 写」与「WriteUserDict 并发调用」引起的包级变量数据竞争。
var systemDictOnce sync.Once

// systemDictDir 已解析的系统级词典目录（仅由 systemDictOnce 写入；空 = 尚未解析）。
var systemDictDir string

// SystemDictDir 返回系统级词典目录（仅解析路径，不物化）。路径解析幂等（sync.Once 单飞）。
// 缓存根不可得（无 HOME 等异常）→ 回落系统临时目录（保证目录恒可解析）。
func SystemDictDir() string {
	systemDictOnce.Do(func() {
		base, err := os.UserCacheDir()
		if err != nil || base == "" {
			base = os.TempDir()
		}
		systemDictDir = filepath.Join(base, "chonkpilot", "vfts", "jieba")
	})
	return systemDictDir
}

// UserDictPath 返回系统级自定义词文件路径。
func UserDictPath() string { return filepath.Join(SystemDictDir(), userDictName) }

// ensureSystemDict 物化内嵌基础词典与自定义词文件到系统级目录，返回目录路径：
//   - 基础词典：缺失或大小不符 → 原子覆盖（内嵌源为准，纠正被篡改者的同时避免重复写 5MB）；
//   - user_dict.txt：缺失 → 建空文件；**已存在则保留**（不覆盖用户编辑）。
func ensureSystemDict() (string, error) {
	dir := SystemDictDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir 系统词典目录 %s: %w", dir, err)
	}
	for _, name := range []string{jieba.DictName, jieba.HMMName} {
		raw, err := jieba.Read(name)
		if err != nil {
			return "", fmt.Errorf("读取内嵌词典 %s: %w", name, err)
		}
		path := filepath.Join(dir, name)
		if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(raw)) {
			continue // 已就位（按大小判等；基础词典只随引擎更新）
		}
		if err := writeFileAtomic(path, raw); err != nil {
			return "", fmt.Errorf("物化基础词典 %s: %w", name, err)
		}
	}
	ud := filepath.Join(dir, userDictName)
	if _, err := os.Stat(ud); errors.Is(err, os.ErrNotExist) {
		if err := writeFileAtomic(ud, []byte{}); err != nil {
			return "", fmt.Errorf("初始化自定义词文件: %w", err)
		}
	}
	return dir, nil
}

// ReadUserDict 读系统级自定义词全文（文件不存在 → 空串）。
func ReadUserDict() (string, error) {
	b, err := os.ReadFile(UserDictPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteUserDict 覆写系统级自定义词（原子落盘；`#` 注释与空行由 jieba 侧自行忽略）。
func WriteUserDict(content string) error {
	if _, err := ensureSystemDict(); err != nil {
		return err
	}
	return writeFileAtomic(UserDictPath(), []byte(content))
}

// userDictWordCount 统计自定义词条数（去空行与 `#` 注释行）。
func userDictWordCount(content string) int {
	n := 0
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		n++
	}
	return n
}

// toolDictGet：查看系统级词典（目录 / 自定义词全文 / 词条数 / 基础词典文件名）。
func toolDictGet(_ context.Context, _ map[string]any) (any, error) {
	if err := ensureZvec(); err != nil {
		return nil, err
	}
	ud, err := ReadUserDict()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"dict_dir":       SystemDictDir(),
		"user_dict_path": UserDictPath(),
		"user_dict":      ud,
		"word_count":     userDictWordCount(ud),
		"base_dicts":     []string{jieba.DictName, jieba.HMMName},
	}, nil
}

// toolDictSet：编辑系统级自定义词（覆写 user_dict.txt）。调用方（插件）负责触发重建索引。
func toolDictSet(_ context.Context, args map[string]any) (any, error) {
	if err := ensureZvec(); err != nil {
		return nil, err
	}
	content := getString(args, "user_dict")
	if err := WriteUserDict(content); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "word_count": userDictWordCount(content)}, nil
}
