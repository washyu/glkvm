#!/bin/sh
# Builds glkvm-agent for Windows x64 and ARM64 into ./dist.
# Needs Go 1.26+ (https://go.dev/dl/); no C compiler or Windows SDK.
set -eu
cd "$(dirname "$0")"
go test ./...
mkdir -p dist
for arch in amd64 arm64; do
	GOOS=windows GOARCH=$arch go build -trimpath -ldflags "-s -w -H windowsgui" -o "dist/glkvm-agent-$arch.exe" .
done
ls -l dist
