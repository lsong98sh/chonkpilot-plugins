# third_party/zvec — vendored zvec C-API 台账

> 本目录是 `src/plugins/vfts`（vfts mcp-server，CGO 链接 zvec C-API）构建/运行所需的 **vendored 头与导入库**。
> 对照 `third_party/jieba`（含 `LICENSE.cppjieba`）补齐本目录的**许可 / 来源 / 版本**台账（审查 F-33 · 视角⑯供应链）。

## 组件

| 文件 | 说明 | 是否入库 |
|---|---|---|
| `include/zvec/c_api.h` | zvec C-API 头（CGO 编译期 `#include`） | ✅ 跟踪 |
| `windows_amd64/zvec_c_api.lib` | Windows/amd64 导入库（链接期） | ✅ 跟踪 |
| `windows_amd64/zvec_c_api.dll` | Windows/amd64 运行库（≈25MB，运行时加载，**须与 vfts 引擎 exe 同目录**） | ✅ **入库**（2026-10-09 决策：仓库自包含） |

## 来源与版本

- 上游：`github.com/zvec-ai/zvec-go`
- 版本：**v0.7.0**
- 版本锚点：与 `src/plugins/vfts/go.mod` 的 `require github.com/zvec-ai/zvec-go v0.7.0` **唯一对齐**——
  本仓跟踪的头/库必须取自同一版本；升级时**此处与 go.mod 两处同步**。

## 许可

- **Apache License 2.0**（见 `include/zvec/c_api.h` 头声明与同目录 `LICENSE.zvec`）。

## 版本一致性校验与升级

头 `c_api.h`、链接库 `zvec_c_api.lib`、运行库 `zvec_c_api.dll` **三者均随源码入库**——clean clone 即可 CGO 构建与运行，`build-vfts.ps1` **无需额外下载步骤**。

1. **版本一致性**：三者必须取自 `github.com/zvec-ai/zvec-go` **同一版本**（本仓 = v0.7.0）——不一致会在链接或运行时加载失败；
2. **升级步骤**：取目标版本 GitHub Releases 资产 `zvec-libs-windows-x64.zip`，替换上述三份文件，并**同步** `src/plugins/vfts/go.mod` 的 `require github.com/zvec-ai/zvec-go` 版本；
3. **完整性**：优先核对 Release 公布的校验值；未公布时以「同版本 Release 资产」为唯一来源。

> **历史沿革**：曾采「`.dll` 不入库 + clean clone 下载步骤」（F-03 决策）；**2026-10-09 用户决策改为入库**（与 D-38 的 `WebView2Loader.dll` 同法——消除 clean clone 的构建前置，使仓库自包含）。
> 完整前置说明见 `docs/spec/00-overview/03-构建与部署.md` §5.2 与 `docs/spec/50-testing/50-测试体系.md` §5.2。
