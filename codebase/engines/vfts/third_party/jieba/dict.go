// Package jieba 内嵌 jieba 分词基础词典（构建期 vendor，随引擎 exe 分发）。
//
// 来源：cppjieba（MIT 许可）词典，经 github.com/yanyiwu/gojieba@v1.4.0 模块取得：
//   - jieba.dict.utf8（主词典，约 5.0MB）
//   - hmm_model.utf8（HMM 词性/新词模型，约 0.5MB）
//
// 许可全文见同目录 LICENSE.cppjieba。**二进制词典文件不得用文本工具编辑/改写**。
//
// 运行时由 server 包在首次初始化 zvec 前物化到**系统级**缓存目录
// （<UserCacheDir>/chonkpilot/vfts/jieba），并 zvec.SetDefaultJiebaDictDir 指向它；
// 引擎建 FTS 索引时再经 extra_params 显式下发同一目录（jieba_dict_dir）。
package jieba

import "embed"

// 内嵌词典文件名（= 同目录实际文件名；zvec JiebaTokenizer 按固定文件名读取）。
const (
	DictName = "jieba.dict.utf8"
	HMMName  = "hmm_model.utf8"
)

//go:embed jieba.dict.utf8 hmm_model.utf8
var files embed.FS

// Read 返回内嵌词典文件字节（name = DictName / HMMName）。
func Read(name string) ([]byte, error) { return files.ReadFile(name) }
