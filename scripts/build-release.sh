#!/usr/bin/env bash
# 交叉编译 7 个平台的 NPA，版本号为 v0.4.1-<构建时刻的 Unix 秒级时间戳>。
#
# 用法：
#   scripts/build-release.sh                 # 输出到 dist/npa-all_<时间戳>/
#   scripts/build-release.sh /path/to/out    # 指定输出目录
#
# 产物：npa/bin/npa-<goos>-<goarch>[.exe] + npa/npa-all_<时间戳>.zip
set -euo pipefail

cd "$(dirname "$0")/.."

BASE_VERSION="0.4.1"
BUILD_STAMP="$(date +%s)"
VERSION="${BASE_VERSION}-${BUILD_STAMP}"
OUT_ROOT="${1:-dist/npa-all_${BUILD_STAMP}}"
PKG_DIR="${OUT_ROOT}/npa"
BIN_DIR="${PKG_DIR}/bin"
LDFLAGS="-s -w -X github.com/npa/npa/internal/service.Version=${VERSION}"

PLATFORMS=(
  darwin/amd64
  darwin/arm64
  linux/amd64
  linux/arm64
  windows/386
  windows/amd64
  windows/arm64
)

rm -rf "${BIN_DIR}"
mkdir -p "${BIN_DIR}"

# 浏览器插件进程（obscura）：放在 plugins/browser/ 下，NPA 启动时按平台挑自己那份。
PLUGIN_DIR="${PKG_DIR}/plugins/browser"
rm -rf "${PLUGIN_DIR}"
mkdir -p "${PLUGIN_DIR}"

# 插件管理器插件进程：放在 plugins/manager/ 下，负责从公有插件仓库下载插件。
MANAGER_DIR="${PKG_DIR}/plugins/manager"
rm -rf "${MANAGER_DIR}"
mkdir -p "${MANAGER_DIR}"

for platform in "${PLATFORMS[@]}"; do
  goos="${platform%%/*}"
  goarch="${platform##*/}"
  target="${BIN_DIR}/npa-${goos}-${goarch}"
  [ "${goos}" = "windows" ] && target="${target}.exe"
  echo "building ${goos}/${goarch} -> ${target}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${target}" ./cmd/npa

  plugin="${PLUGIN_DIR}/npa-obscura-${goos}-${goarch}"
  [ "${goos}" = "windows" ] && plugin="${plugin}.exe"
  echo "building plugin ${goos}/${goarch} -> ${plugin}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${plugin}" ./cmd/npa-obscura

  manager="${MANAGER_DIR}/npa-plugin-manager-${goos}-${goarch}"
  [ "${goos}" = "windows" ] && manager="${manager}.exe"
  echo "building plugin manager ${goos}/${goarch} -> ${manager}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${manager}" ./cmd/npa-plugin-manager
done

# 可选 UPX 压缩：能压就压；本架构可执行的必须能跑（防 upx 5.3.0 --force-macos
# 产出启动即崩的二进制），跑崩（rc>=128）自动还原；交叉目标跑不了（rc=126）
# 信任 upx 的压缩结果。mac x86_64 需 UPX=4.2.4 路径：NPA_UPX=/path/to/upx scripts/build-release.sh
UPX_BIN="${NPA_UPX:-upx}"
if command -v "$UPX_BIN" >/dev/null 2>&1; then
  for f in "${BIN_DIR}"/*; do
    [ -f "$f" ] || continue
    cp "$f" "$f.upxbak"
    if { "$UPX_BIN" -9 "$f" >/dev/null 2>&1 || "$UPX_BIN" --force-macos -9 "$f" >/dev/null 2>&1; } \
      && [ $(($(wc -c < "$f"))) -lt $(($(wc -c < "$f.upxbak"))) ]; then
      rc=0
      "$f" --version >/dev/null 2>&1 || rc=$?
      if [ "$rc" -ge 128 ]; then
        mv "$f.upxbak" "$f"
        echo "upx: smoke failed (rc=$rc), restored: $(basename "$f")"
        continue
      fi
      rm -f "$f.upxbak"
      echo "upx: packed $(basename "$f") -> $(wc -c < "$f") bytes"
    else
      mv "$f.upxbak" "$f"
      echo "upx: skipped (not compressible): $(basename "$f")"
    fi
  done
else
  echo "upx: not found, skip packing"
fi

# 本机平台的二进制拿来验证版本注入是否生效。
host_goos="$(go env GOOS)"
host_goarch="$(go env GOARCH)"
host_bin="${BIN_DIR}/npa-${host_goos}-${host_goarch}"
[ "${host_goos}" = "windows" ] && host_bin="${host_bin}.exe"
echo "version check: $("${host_bin}" --version)"

if [ -f assets/使用说明.txt ]; then
  sed "s/__NPA_VERSION__/${VERSION}/g" assets/使用说明.txt > "${PKG_DIR}/使用说明.txt"
fi

# run 启动脚本——自动检测平台
cat > "${PKG_DIR}/run.sh" <<'RUNSH'
#!/bin/bash
# NPA 一键启动——自动检测平台
DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$DIR/bin"
DATA_DIR="$DIR/data"
OS="$(uname -s)"; ARCH="$(uname -m)"
case "$OS/$ARCH" in
  Linux/x86_64)  BIN="npa-linux-amd64" ;;
  Linux/aarch64|Linux/arm64) BIN="npa-linux-arm64" ;;
  Darwin/x86_64) BIN="npa-darwin-amd64" ;;
  Darwin/arm64|Darwin/aarch64) BIN="npa-darwin-arm64" ;;
  *) echo "不支持: $OS/$ARCH"; exit 1 ;;
esac
[ ! -f "$BIN_DIR/$BIN" ] && echo "找不到: $BIN_DIR/$BIN" && exit 1
chmod +x "$BIN_DIR/$BIN"; mkdir -p "$DATA_DIR"
echo "启动: $BIN (数据: $DATA_DIR)"
exec "$BIN_DIR/$BIN" --data-dir "$DATA_DIR"
RUNSH
chmod +x "${PKG_DIR}/run.sh"

cat > "${PKG_DIR}/run.bat" <<'RUNBAT'
@echo off
rem NPA 一键启动——自动检测平台
setlocal
set DIR=%~dp0
set BIN_DIR=%DIR%bin
set DATA_DIR=%DIR%data
if /i "%PROCESSOR_ARCHITECTURE%"=="AMD64" (set BIN=npa-windows-amd64.exe) else if /i "%PROCESSOR_ARCHITECTURE%"=="ARM64" (set BIN=npa-windows-arm64.exe) else (set BIN=npa-windows-386.exe)
if not exist "%BIN_DIR%\%BIN%" (echo 找不到: %BIN_DIR%\%BIN% & exit /b 1)
if not exist "%DATA_DIR%" mkdir "%DATA_DIR%"
echo 启动: %BIN% (数据: %DATA_DIR%)
"%BIN_DIR%\%BIN%" --data-dir "%DATA_DIR%"
RUNBAT

ZIP_PATH="${PKG_DIR}/npa-all_${BUILD_STAMP}.zip"
python3 - "$PKG_DIR" "$ZIP_PATH" <<'PY'
import os, sys, zipfile

pkg_dir, zip_path = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as archive:
    for root, _dirs, files in os.walk(pkg_dir):
        for name in sorted(files):
            if name.endswith(".zip"):
                continue
            full = os.path.join(root, name)
            # UTF-8 文件名标志（0x800），否则 Windows 解压中文名乱码
            info = zipfile.ZipInfo.from_file(full, os.path.relpath(full, os.path.dirname(pkg_dir)))
            info.flag_bits |= 0x800
            info.compress_type = zipfile.ZIP_DEFLATED
            with open(full, "rb") as handle, archive.open(info, "w") as target:
                while chunk := handle.read(1 << 20):
                    target.write(chunk)
print("zip:", zip_path)
PY

if command -v shasum >/dev/null 2>&1; then
  shasum -a 256 "${ZIP_PATH}"
fi
echo "done: ${VERSION}"
