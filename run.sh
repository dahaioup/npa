#!/usr/bin/env sh
# NPA 启动脚本
#
# 默认：前台阻塞执行，输出同时进入控制台与日志文件（<数据目录>/npa.log）。
# 若存在 <数据目录>/hide_console 标记（在 Web 设置里开启"隐藏控制台"）：
#   改为后台异步启动，输出只进日志文件，启动后本控制台可以直接关闭。
#
# 用法：./run.sh [--data-dir DIR] [--log-level LEVEL]

DIR=$(cd "$(dirname "$0")" && pwd)

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$OS" in
  darwin|linux) ;;
  msys*|mingw*|cygwin*) OS=windows ;;
  *) echo "不支持的操作系统: $OS" >&2; exit 1 ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  i386|i686|x86) ARCH=386 ;;
  *) echo "不支持的架构: $ARCH" >&2; exit 1 ;;
esac

BIN="$DIR/bin/npa-$OS-$ARCH"
if [ ! -f "$BIN" ]; then
  # 打包布局没有时，回退到同目录的裸二进制（仓库里直接跑 ./npa 的场景）
  if [ -f "$DIR/npa" ]; then
    BIN="$DIR/npa"
  elif [ -f "$DIR/npa.exe" ]; then
    BIN="$DIR/npa.exe"
  else
    echo "缺少二进制: $BIN（也没有同目录的 ./npa）" >&2
    exit 1
  fi
fi
chmod +x "$BIN" 2>/dev/null || true

# 数据目录：默认 ~/.npa，跟随 --data-dir 参数
DATA_DIR="${HOME}/.npa"
PREV=""
for arg in "$@"; do
  if [ "$PREV" = "--data-dir" ]; then
    DATA_DIR="$arg"
  fi
  PREV="$arg"
done
MARKER="$DATA_DIR/hide_console"
LOG="$DATA_DIR/npa.log"

if [ -f "$MARKER" ]; then
  mkdir -p "$DATA_DIR" 2>/dev/null || true
  nohup "$BIN" "$@" >/dev/null 2>&1 &
  disown 2>/dev/null || true
  echo "NPA 已在后台启动，本控制台可以关闭"
  echo "日志: $LOG"
  echo "Web : http://127.0.0.1:56700"
  exit 0
fi

echo "NPA 启动中: $(basename "$BIN")（前台运行，输出同时进入控制台与 $LOG）"
echo "Web: http://127.0.0.1:56700"
exec "$BIN" "$@"
