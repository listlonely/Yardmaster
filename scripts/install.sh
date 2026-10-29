#!/usr/bin/env bash
# yardmaster Linux 安装脚本（M2-W5）
#
# 用法:
#   ./install.sh                       # 最新 release → /usr/local/bin
#   ./install.sh v0.2.0                # 指定版本
#   ./install.sh --user                # 安装到 ~/.local/bin（无需 root）
#   ./install.sh --file yardmaster-linux-amd64.tar.gz   # 离线安装
#
# 数据目录固定 ~/.yardmaster；卸载只需删除安装的二进制与数据目录。
set -euo pipefail
REPO="listlonely/Yardmaster"
DEST="/usr/local/bin"
VERSION=""
FILE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --user) DEST="$HOME/.local/bin"; shift ;;
    --file) FILE="$2"; shift 2 ;;
    -h|--help) echo "用法: install.sh [版本] [--user] [--file <tar.gz>]"; exit 0 ;;
    *) VERSION="$1"; shift ;;
  esac
done

case "$(uname -m)" in
  x86_64) ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *) echo "不支持的架构: $(uname -m)（仅 amd64/arm64）" >&2; exit 1 ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if [ -n "$FILE" ]; then
  TGZ="$FILE"
  echo "离线安装：$TGZ"
else
  if [ -z "$VERSION" ]; then
    VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
      grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4)"
  fi
  URL="https://github.com/$REPO/releases/download/$VERSION/yardmaster-linux-$ARCH.tar.gz"
  echo "下载 $URL"
  TGZ="$TMP/yardmaster.tar.gz"
  curl -fsSL "$URL" -o "$TGZ"
fi

tar xzf "$TGZ" -C "$TMP"
SRC="$TMP/yardmaster-linux-$ARCH/yardmaster"
[ -f "$SRC" ] || { echo "包内未找到 yardmaster（期望 $SRC）" >&2; exit 1; }

mkdir -p "$DEST"
install -m 0755 "$SRC" "$DEST/yardmaster"
"$DEST/yardmaster" version
echo "安装完成 → $DEST/yardmaster"
echo "启动: yardmaster serve（默认监听 http://127.0.0.1:8317，数据目录 ~/.yardmaster）"
