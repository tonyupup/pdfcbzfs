#!/bin/sh
# Build the render helper and package it with PDFium (BSD licensed prebuilt
# library from bblanchon/pdfium-binaries, pinned so builds are reproducible).
#
#   helper/build.sh arm64|amd64 OUTDIR
#
# Result: OUTDIR/pdfcbzfs-render-linux-ARCH.tar.gz holding
#   bin/pdfcbzfs-render   the helper (finds ../lib/libpdfium.so through RUNPATH)
#   lib/libpdfium.so
#   licenses/             PDFium and its third-party licenses
# Unpack it into the same prefix as the pdfcbzfs binary (PREFIX/bin/pdfcbzfs
# next to PREFIX/bin/pdfcbzfs-render); pdfcbzfs then finds the helper by itself.
#
# arm64 needs aarch64-linux-gnu-gcc when built on another architecture.
set -eu

arch=${1:?usage: build.sh arm64|amd64 OUTDIR}
out=${2:?usage: build.sh arm64|amd64 OUTDIR}
release=${PDFIUM_RELEASE:-chromium/8125}

case $arch in
arm64) pdfium=arm64; cc=${CC:-aarch64-linux-gnu-gcc}; strip=${STRIP:-aarch64-linux-gnu-strip} ;;
amd64) pdfium=x64; cc=${CC:-gcc}; strip=${STRIP:-strip} ;;
*) echo "unsupported architecture: $arch" >&2; exit 2 ;;
esac

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

curl -fsSL -o "$work/pdfium.tgz" \
	"https://github.com/bblanchon/pdfium-binaries/releases/download/$release/pdfium-linux-$pdfium.tgz"
mkdir "$work/pdfium"
tar xzf "$work/pdfium.tgz" -C "$work/pdfium"

stage="$work/stage"
mkdir -p "$stage/bin" "$stage/lib" "$stage/licenses"
"$cc" -O2 -Wall -Wextra -o "$stage/bin/pdfcbzfs-render" "$here/render.c" \
	-I"$work/pdfium/include" -L"$work/pdfium/lib" -lpdfium -lm -Wl,-rpath,'$ORIGIN/../lib'
"$strip" "$stage/bin/pdfcbzfs-render"
cp "$work/pdfium/lib/libpdfium.so" "$stage/lib/"
cp -r "$work/pdfium/licenses/." "$stage/licenses/"
cp "$work/pdfium/LICENSE" "$stage/licenses/PDFium-LICENSE"

mkdir -p "$out"
tar czf "$out/pdfcbzfs-render-linux-$arch.tar.gz" -C "$stage" bin lib licenses
ls -l "$out/pdfcbzfs-render-linux-$arch.tar.gz"
