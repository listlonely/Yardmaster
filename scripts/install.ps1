# yardmaster Windows 安装脚本（M2-W5）
#
# 用法:
#   irm 脚本后执行（示例）:  powershell -File install.ps1
#   .\install.ps1                              # GitHub 最新 release
#   .\install.ps1 -Version v0.2.0              # 指定版本
#   .\install.ps1 -File yardmaster-windows-amd64.zip   # 离线安装本地包
#   .\install.ps1 -InstallDir D:\tools\ym      # 自定义安装目录
#
# 行为：下载（或使用本地包）→ 解压 yardmaster.exe 到安装目录 →
# 加入用户 PATH（已含则跳过）。数据目录固定 ~\.yardmaster，卸载
# 只需删除安装目录与数据目录。
param(
  [string]$Version = "",
  [string]$File = "",
  [string]$InstallDir = "$env:LOCALAPPDATA\Programs\yardmaster",
  [switch]$NoPath   # 跳过写入用户 PATH（测试/绿色安装用）
)
$ErrorActionPreference = "Stop"
$repo = "listlonely/Yardmaster"

if (-not [Environment]::Is64BitOperatingSystem) { throw "仅支持 64 位 Windows" }
$arch = "amd64"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("ym-install-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  if ($File -ne "") {
    $zip = (Resolve-Path $File).Path
    Write-Host "离线安装：$zip"
  } else {
    if ($Version -eq "") {
      $rel = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"
      $Version = $rel.tag_name
    }
    $url = "https://github.com/$repo/releases/download/$Version/yardmaster-windows-$arch.zip"
    Write-Host "下载 $url"
    $zip = Join-Path $tmp "yardmaster.zip"
    Invoke-WebRequest -Uri $url -OutFile $zip
  }
  Expand-Archive -Path $zip -DestinationPath $tmp -Force
  $src = Join-Path $tmp "yardmaster-windows-$arch\yardmaster.exe"
  if (-not (Test-Path $src)) { throw "包内未找到 yardmaster.exe（期望 $src）" }

  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
  Copy-Item $src (Join-Path $InstallDir "yardmaster.exe") -Force

  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  if ($NoPath) {
    Write-Host "跳过 PATH 写入（-NoPath）"
  } elseif (($userPath -split ";") -notcontains $InstallDir) {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$InstallDir", "User")
    Write-Host "已把 $InstallDir 加入用户 PATH（对新建终端生效）"
  }
  $v = & (Join-Path $InstallDir "yardmaster.exe") version
  Write-Host "安装完成：$v"
  Write-Host "  可执行: $InstallDir\yardmaster.exe"
  Write-Host "  启动:   yardmaster serve（默认监听 http://127.0.0.1:8317，数据目录 ~\.yardmaster）"
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
