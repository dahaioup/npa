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

for platform in "${PLATFORMS[@]}"; do
  goos="${platform%%/*}"
  goarch="${platform##*/}"
  target="${BIN_DIR}/npa-${goos}-${goarch}"
  [ "${goos}" = "windows" ] && target="${target}.exe"
  echo "building ${goos}/${goarch} -> ${target}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${target}" ./cmd/npa
done

# 本机平台的二进制拿来验证版本注入是否生效。
host_goos="$(go env GOOS)"
host_goarch="$(go env GOARCH)"
host_bin="${BIN_DIR}/npa-${host_goos}-${host_goarch}"
[ "${host_goos}" = "windows" ] && host_bin="${host_bin}.exe"
echo "version check: $("${host_bin}" --version)"

if [ -f assets/使用说明.txt ]; then
  sed "s/__NPA_VERSION__/${VERSION}/g" assets/使用说明.txt > "${PKG_DIR}/使用说明.txt"
fi

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
