#!/bin/bash
# Build fnOS .fpk packages (x86 + arm) for 批量解压.
#
# Requirements: Go 1.24+, curl, tar, md5sum
# Usage: ./build.sh [version]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
VERSION="${1:-$(grep '^version' "$ROOT/fnos/manifest" | awk -F= '{print $2}' | tr -d ' ')}"
SEVENZ_VER="2409"
CACHE="$ROOT/build/cache"
DIST="$ROOT/dist"
mkdir -p "$CACHE" "$DIST"

get_7zz() { # $1 = x64|arm64
    local out="$CACHE/7zz-$1"
    if [ ! -x "$out" ]; then
        echo "==> 下载 7-Zip ($1)"
        curl -fL --retry 3 -o "$CACHE/7z-$1.tar.xz" \
            "https://github.com/ip7z/7zip/releases/download/${SEVENZ_VER:0:2}.${SEVENZ_VER:2}/7z${SEVENZ_VER}-linux-$1.tar.xz"
        mkdir -p "$CACHE/x-$1" && tar -xJf "$CACHE/7z-$1.tar.xz" -C "$CACHE/x-$1" 7zz
        install -m 755 "$CACHE/x-$1/7zz" "$out"
    fi
    echo "$out"
}

build_one() { # $1 = fnOS platform (x86|arm)  $2 = GOARCH  $3 = 7-Zip arch
    local platform="$1" goarch="$2" zarch="$3"
    local work="$ROOT/build/$platform"
    rm -rf "$work" && mkdir -p "$work/app/bin" "$work/pkg"
    echo "==> 编译 $platform ($goarch)"
    (cd "$ROOT/src" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -mod=vendor -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" -o "$work/app/bin/batch-unzip" .)
    install -m 755 "$(get_7zz "$zarch")" "$work/app/bin/7zz"
    cp -a "$ROOT/fnos/ui" "$work/app/ui"
    (cd "$work/app" && tar --owner=0 --group=0 -czf "$work/pkg/app.tgz" .)

    local pkg="$work/pkg"
    cp -a "$ROOT/fnos/cmd" "$ROOT/fnos/config" "$ROOT/fnos/ui" "$ROOT/fnos/wizard" "$pkg/"
    cp "$ROOT"/fnos/*.sc "$ROOT"/fnos/ICON*.PNG "$pkg/"
    chmod 755 "$pkg"/cmd/*
    local sum; sum=$(md5sum "$pkg/app.tgz" | cut -d' ' -f1)
    sed -e "s/^version .*/version         = $VERSION/" \
        -e "s/^platform .*/platform        = $platform/" \
        -e "s/^checksum .*/checksum        = $sum/" "$ROOT/fnos/manifest" > "$pkg/manifest"
    local fpk="$DIST/batchunzip_${VERSION}_${platform}.fpk"
    (cd "$pkg" && tar --owner=0 --group=0 -czf "$fpk" *)
    echo "==> 完成 $fpk ($(du -h "$fpk" | cut -f1))"
}

build_one x86 amd64 x64
build_one arm arm64 arm64
