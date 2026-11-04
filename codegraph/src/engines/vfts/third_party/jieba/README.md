# third_party/jieba — vendored 分词词典台账

> 本目录是 `chonkpilot-plugins/codegraph/src/engines/vfts`（vfts 引擎，CGO 链接 zvec C-API）建 FTS 索引所需的中文分词**词典数据快照**（非 Go module、非代码）。
> 与同层 `third_party/zvec` 同格式，补齐**许可 / 来源 / 版本**台账（审查 F-33 · 视角⑯供应链）。

## 组件

| 文件 | 说明 | 是否入库 |
|---|---|---|
| `jieba.dict.utf8` | 主词典（5,071,204 B ≈ 5.0 MB，348,982 行；每行 `词 频次 词性`） | ✅ 跟踪 |
| `hmm_model.utf8` | HMM 新词/未登录词模型（519,739 B ≈ 0.5 MB） | ✅ 跟踪 |
| `dict.go` | `//go:embed` 封装（`DictName` / `HMMName` / `Read`），随引擎 exe 分发 | ✅ 跟踪 |
| `LICENSE.cppjieba` | 上游许可全文（MIT） | ✅ 跟踪 |

## 来源与版本

- 上游词典：**cppjieba**（`github.com/yanyiwu/cppjieba` 的 `dict/` 目录，MIT）
- 取得途径：经 `github.com/yanyiwu/gojieba@v1.4.0` 模块内的同名词典文件取出（见 `dict.go` 文件头）
- 版本：**数据快照，无独立版本号** → 版本锚点 = **`gojieba@v1.4.0`** 内 `dict/jieba.dict.utf8` / `dict/hmm_model.utf8`
- 构建期依赖：**无**（本目录仅内嵌数据，`codegraph/src/engines/vfts/go.mod` 不 require `gojieba`）

## 许可

- **MIT License**（见同目录 `LICENSE.cppjieba`，Copyright (c) 2013）。

## 运行时物化与版本一致性校验

词典经 `//go:embed` 编进引擎 exe，首次初始化 zvec 前由 server 物化到**系统级**缓存目录
（`<UserCacheDir>/chonkpilot/vfts/jieba`），并由 `zvec.SetDefaultJiebaDictDir` 指向；建 FTS 索引时再经
`extra_params` 显式下发同一目录（`jieba_dict_dir`）。详见 `dict.go` 文件头。

1. **文件名固定**：zvec `JiebaTokenizer` 按固定文件名读取（`jieba.dict.utf8` / `hmm_model.utf8`），**不得改名**；
2. **禁止文本编辑**：二进制/大体积词典不得用文本工具改写，升级须**整份替换**；
3. **升级步骤**：取目标 `gojieba` 版本的 `dict/` 同名文件整份替换，并核对字节数 / 行数（当前主词典 5,071,204 B / 348,982 行）；
4. **一致性**：`jieba.dict.utf8` 与 `hmm_model.utf8` 必须取自**同一版本**，混用会导致分词结果与索引不一致。

> **历史沿革**：词典随 `src/plugins/vfts` 建 FTS 索引引入（2026-10-06，`dict.go` 记录「构建期 vendor，随引擎 exe 分发」）；2026-10-25 引擎源码随插件迁入 `chonkpilot-plugins/codegraph/engines/vfts`；2026-10-30 补本台账。
