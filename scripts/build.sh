#!/usr/bin/env bash
# 构建插件动态库：先跑测试，再产出目标平台的 .so/.dll/.dylib。
#
# 本机没有 Go 工具链（只有 docker 镜像里有），因此统一经容器构建。
# 容器参数（镜像、挂载、GOCACHE 卷）全部收敛在 godocker.sh，本脚本只编排
# 「先 test 再 build」的顺序，避免两处各写一份 docker 命令。
#
# **不要用 alpine 版**：宿主 CPA 镜像是 Debian(glibc)，alpine(musl) 构建出的
# .so 会以 "libc.musl-aarch64.so.1: cannot open shared object file" 加载失败。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GODOCKER="$ROOT/scripts/godocker.sh"

# 产物名必须与 pluginID 一致（freetier2api-fork）。
# -s -w 去掉符号表与 DWARF：约 12.5MB -> 9.1MB（-27%）。
# 代价是 panic 堆栈只剩函数名与地址，没有文件名/行号 —— 定位线上问题时
# 需要先用这份源码在本地复现，或临时用不带该参数的构建还原堆栈。
# 宿主容器里没有 strip 命令，因此这一步必须在构建期完成，无法事后剥离。
LDFLAGS="-s -w"

mkdir -p "$ROOT/dist"

# 第一步：全量测试。
"$GODOCKER" test ./... -count=1 -timeout 300s

# 第二步：按目标平台决定扩展名后构建 c-shared 动态库。
ext="so"
case "${GOOS:-linux}" in
  windows) ext="dll" ;;
  darwin) ext="dylib" ;;
esac

"$GODOCKER" build -buildmode=c-shared -ldflags="$LDFLAGS" -o "dist/freetier2api-fork.$ext" .

echo "Built dist/freetier2api-fork.$ext"
