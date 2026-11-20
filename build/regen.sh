#!/bin/sh
# Regenerates the Go of the portable engine from build/libvpx/kolovpx.wasm.
# Usage: regen.sh [rootdir]   (rootdir defaults to this repository)
# wasm2go is pinned in tools/go.mod and needs a warm module cache or the network. shardgen
# (tools/shardgen) splits its single package into core and SHARDS shard packages.
set -eu

SHARDS=6
here=$(cd "$(dirname "$0")" && pwd)
root=${1:-$(dirname "$here")}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
base=github.com/ciranda-dev/kolo-video/vpx/internal/portable/wasm
dst=$root/vpx/internal/portable

(cd "$here/tools" && GOFLAGS= GOWORK=off go tool wasm2go -unsafe -pkg portable -o "$tmp/libvpx.go" "$here/libvpx/kolovpx.wasm")
first=$(head -n 1 "$tmp/libvpx.go")
echo "$first" | grep -Eq '^// Code generated .* DO NOT EDIT\.$' || { echo "regen.sh: wasm2go wrote no generated-code header" >&2; exit 1; }
header=$(echo "$first" | sed 's/\. DO NOT EDIT\.$/ and build\/tools\/shardgen. DO NOT EDIT./')
(cd "$here/tools" && GOFLAGS= GOWORK=off go run ./shardgen -in "$tmp/libvpx.go" -out "$tmp/gen" -pkg portable \
	-base "$base" -shards "$SHARDS" -header "$header")

(cd "$tmp/gen" && find . -type f | sort | sed 's|^\./||') >"$tmp/files"
while read -r f; do
	head -n 1 "$tmp/gen/$f" | grep -Eq '^// Code generated .* DO NOT EDIT\.$' || { echo "regen.sh: $f has no generated-code header" >&2; exit 1; }
	[ -z "$(gofmt -l "$tmp/gen/$f")" ] || { echo "regen.sh: $f is not gofmt-clean" >&2; exit 1; }
	mkdir -p "$(dirname "$dst/$f")"
	if cmp -s "$tmp/gen/$f" "$dst/$f"; then echo "$f: unchanged"; continue; fi
	cp "$tmp/gen/$f" "$dst/$f.new"
	mv "$dst/$f.new" "$dst/$f"
	echo "$f: written"
done <"$tmp/files"

if [ -d "$dst/wasm" ]; then
	(cd "$dst" && find wasm -type f -name "*.go" | sort) | while read -r f; do
		grep -qx "$f" "$tmp/files" || { rm "$dst/$f"; echo "$f: removed"; }
	done
	find "$dst/wasm" -type d -empty -delete
fi
