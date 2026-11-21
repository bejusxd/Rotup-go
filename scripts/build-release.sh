#!/bin/sh
# Build self-contained ROTUP release archives for Linux amd64 and arm64.
#
#   scripts/build-release.sh [version]      # default: git describe
#
# Output goes to dist/. The binaries are static (CGO disabled; SQLite is pure
# Go) and embed the committed web/dist bundle, so the target host needs no Go,
# no Node and no C library beyond what any Linux has.
set -eu

cd "$(dirname "$0")/.."
VERSION=${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
OUT=dist
rm -rf "$OUT"
mkdir -p "$OUT"

for arch in amd64 arm64; do
	name=rotup-$VERSION-linux-$arch
	dir=$OUT/$name
	mkdir -p "$dir"
	echo "building $name"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$VERSION" -o "$dir/rotupd" ./cmd/rotupd
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-ldflags "-s -w" -o "$dir/rotup-mounthelper" ./cmd/rotup-mounthelper
	cp packaging/install.sh packaging/uninstall.sh "$dir/"
	cp packaging/INSTALL.md "$dir/README.md"
	chmod 0755 "$dir/rotupd" "$dir/rotup-mounthelper" "$dir/install.sh" "$dir/uninstall.sh"
	tar -C "$OUT" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
	rm -rf "$dir"
done

(cd "$OUT" && sha256sum ./*.tar.gz > SHA256SUMS)
ls -l "$OUT"
