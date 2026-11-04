# build.ps1 —— 构建 Remote 三件套（H5 → Remote Server → Remote 插件）。
#
# 产物（本目录 dist/）：
#   chonkpilot-remote-server.exe   # 中央服务（H5 静态资源已 embed）
#   chonkpilot-plugin-remote.exe   # 桌面插件（由主仓 build-plugins.ps1 另建一份到 dist/plugin-apps）
#
# 用法：.\build.ps1 [-SkipWeb] [-WithFake]
param([switch]$SkipWeb, [switch]$WithFake)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$env:GOROOT = "e:\GoDev\go1.26"
$env:Path = (Join-Path $env:GOROOT "bin") + ";" + $env:Path
$env:GOPROXY = "https://goproxy.cn,direct"
$env:GOTOOLCHAIN = "local"

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$out = Join-Path $root "dist"
New-Item -ItemType Directory -Force -Path $out | Out-Null

if (-not $SkipWeb) {
    Write-Host "==> [1/3] web (npm install + build -> server/web/dist)"
    Push-Location (Join-Path $root "server\web")
    try {
        if (-not (Test-Path "node_modules")) { npm install }
        npm run build
        if ($LASTEXITCODE -ne 0) { throw "npm run build failed" }
    } finally { Pop-Location }
}

Write-Host "==> [2/3] remote server"
Push-Location (Join-Path $root "server")
try {
    go build -o (Join-Path $out "chonkpilot-remote-server.exe") .
    if ($LASTEXITCODE -ne 0) { throw "build server failed" }
} finally { Pop-Location }

Write-Host "==> [3/3] remote plugin"
Push-Location (Join-Path $root "client")
try {
    go build -o (Join-Path $out "chonkpilot-plugin-remote.exe") .
    if ($LASTEXITCODE -ne 0) { throw "build plugin failed" }
} finally { Pop-Location }

if ($WithFake) {
    Write-Host "==> [test] fakedevice（仅本地联调，不随产品投放）"
    Push-Location (Join-Path $root "server")
    try {
        go build -o (Join-Path $out "fakedevice.exe") ./cmd/fakedevice
        if ($LASTEXITCODE -ne 0) { throw "build fakedevice failed" }
    } finally { Pop-Location }
}

Write-Host "==> done: $out"
