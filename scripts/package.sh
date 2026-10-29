#!/usr/bin/env bash
# release 打包脚本：由 release workflow 调用。xtrace 日志写入
# pack/package.log，失败时随 ci-debug 分支可读（CI 日志需鉴权拿不到）。
set -x
cd "$(dirname "$0")/../pack" || exit 9

for p in agenthub_linux-amd64 agenthub_linux-arm64 agenthub_windows-amd64.exe; do
  base=${p%.exe}; base=${base#agenthub_} # linux-amd64 / windows-amd64
  dir="agenthub-$base" # 目录名用连字符：避免与二进制文件重名（mkdir File exists）
  mkdir -p "$dir" || exit 1
  cp "$p" "$dir/agenthub${p#agenthub_$base}" || exit 1
  cp ../README.md "$dir/" || exit 1
done

tar czf agenthub-linux-amd64.tar.gz agenthub-linux-amd64 || exit 1
tar czf agenthub-linux-arm64.tar.gz agenthub-linux-arm64 || exit 1
python3 -m zipfile -c agenthub-windows-amd64.zip agenthub-windows-amd64/ || exit 1

rm -rf agenthub-linux-amd64 agenthub-linux-arm64 agenthub-windows-amd64
rm -f agenthub_linux-amd64 agenthub_linux-arm64 agenthub_windows-amd64.exe

sha256sum agenthub-*.tar.gz agenthub-*.zip > SHA256SUMS || exit 1
ls -l
