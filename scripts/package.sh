#!/usr/bin/env bash
# release 打包脚本：由 release workflow 调用。xtrace 日志写入
# pack/package.log，失败时随 ci-debug 分支可读（CI 日志需鉴权拿不到）。
set -x
cd "$(dirname "$0")/../pack" || exit 9

for p in yardmaster_linux-amd64 yardmaster_linux-arm64 yardmaster_windows-amd64.exe; do
  base=${p%.exe}; base=${base#yardmaster_} # linux-amd64 / windows-amd64
  dir="yardmaster-$base" # 目录名用连字符：避免与二进制文件重名（mkdir File exists）
  mkdir -p "$dir" || exit 1
  cp "$p" "$dir/yardmaster${p#yardmaster_$base}" || exit 1
  cp ../README.md "$dir/" || exit 1
done

tar czf yardmaster-linux-amd64.tar.gz yardmaster-linux-amd64 || exit 1
tar czf yardmaster-linux-arm64.tar.gz yardmaster-linux-arm64 || exit 1
python3 -m zipfile -c yardmaster-windows-amd64.zip yardmaster-windows-amd64/ || exit 1

rm -rf yardmaster-linux-amd64 yardmaster-linux-arm64 yardmaster-windows-amd64
rm -f yardmaster_linux-amd64 yardmaster_linux-arm64 yardmaster_windows-amd64.exe

sha256sum yardmaster-*.tar.gz yardmaster-*.zip > SHA256SUMS || exit 1
ls -l
