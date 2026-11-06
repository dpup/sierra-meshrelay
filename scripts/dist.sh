#!/bin/sh
# Builds release archives of meshbridge into dist/: one per target, plus
# SHA256SUMS. Asset names don't include the version, so the newest release
# is always at .../releases/latest/download/meshbridge_<os>_<arch>.tar.gz.
#
#   scripts/dist.sh v0.4.0
set -eu
version=${1:?usage: scripts/dist.sh <version>}
out=$(pwd)/dist
rm -rf "$out"
mkdir -p "$out"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for t in linux/amd64 linux/arm64 linux/arm/6 linux/arm/7 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
	os=$(echo "$t" | cut -d/ -f1)
	arch=$(echo "$t" | cut -d/ -f2)
	arm=$(echo "$t" | cut -s -d/ -f3)
	name="meshbridge_${os}_${arch}${arm:+v$arm}"
	stage="$work/$name"
	mkdir -p "$stage"
	bin=meshbridge
	[ "$os" = windows ] && bin=meshbridge.exe
	GOOS=$os GOARCH=$arch GOARM=$arm CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/$bin" ./cmd/meshbridge
	cp README.md LICENSE "$stage/"
	if [ "$os" = windows ]; then
		if command -v zip >/dev/null; then
			(cd "$stage" && zip -q -r "$out/$name.zip" .)
		else
			(cd "$stage" && python3 -m zipfile -c "$out/$name.zip" ./*)
		fi
	else
		tar -C "$stage" -czf "$out/$name.tar.gz" .
	fi
	echo "built $name"
done
(cd "$out" && sha256sum meshbridge_* > SHA256SUMS)
