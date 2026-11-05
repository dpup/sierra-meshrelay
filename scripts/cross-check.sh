#!/bin/sh
# Builds meshbridge for each GOOS/GOARCH[/GOARM] target given as an argument.
set -e
for t in "$@"; do
	os=$(echo "$t" | cut -d/ -f1)
	arch=$(echo "$t" | cut -d/ -f2)
	arm=$(echo "$t" | cut -s -d/ -f3)
	echo "$t"
	GOOS=$os GOARCH=$arch GOARM=$arm CGO_ENABLED=0 go build -o /dev/null ./cmd/meshbridge
done
