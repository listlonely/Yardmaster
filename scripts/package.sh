#!/usr/bin/env bash
# release 打包脚本：由 release workflow 调用。xtrace 日志写入
# pack/package.log，失败时随 ci-debug 分支可读（CI 日志需鉴权拿不到）。
set -x
cd "$(dirname "$0")/../pack" || exit 9

for p in agenthub_linux-amd64 agenthub_linux-arm64 agenthub_windows-amd64.exe; do
  base=${p%.exe}; base=${base#agenthub_} # linux-amd64 / windows-amd64
  mkdir -p "agenthub_$base" || exit 1
  cp "$p" "agenthub_$base/agenthub${p#agenthub_$base}" || exit 1
  cp ../README.md "agenthub_$base/" || exit 1
done

tar czf agenthub_linux-amd64.tar.gz agenthub_linux-amd64 || exit 1
tar czf agenthub_linux-arm64.tar.gz agenthub_linux-arm64 || exit 1
python3 -m zipfile -c agenthub_windows-amd64.zip agenthub_windows-amd64/ || exit 1

rm -rf agenthub_linux-amd64 agenthub_linux-arm64 agenthub_windows-amd64
rm -f agenthub_linux-amd64 agenthub_linux-arm64 agenthub_windows-amd64.exe

sha256sum agenthub_*.tar.gz agenthub_*.zip > SHA256SUMS || exit 1
ls -l
