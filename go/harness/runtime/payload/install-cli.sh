#!/bin/sh
set -eu
provider=$1
arch=$2
destination=$3
lock=$4
version=$(jq -er --arg provider "$provider" '.[$provider].version' "$lock")
checksum=$(jq -er --arg provider "$provider" --arg arch "$arch" '.[$provider].checksums[$arch]' "$lock")
mkdir -p "$destination/bin"
case "$provider/$arch" in
 claude/amd64) platform=linux-x64-musl ;;
 claude/arm64) platform=linux-arm64-musl ;;
 codex/amd64) platform=x86_64-unknown-linux-musl ;;
 codex/arm64) platform=aarch64-unknown-linux-musl ;;
 *) echo "unsupported runtime provider/platform: $provider/$arch" >&2; exit 1 ;;
esac
if [ "$provider" = claude ]; then
 wget -T 60 -t 3 -O "$destination/bin/claude" "https://downloads.claude.ai/claude-code-releases/$version/$platform/claude"
 echo "$checksum  $destination/bin/claude" | sha256sum -c -
 chmod 0755 "$destination/bin/claude"
else
 archive=$(mktemp)
 trap 'rm -f "$archive"' EXIT
 wget -T 60 -t 3 -O "$archive" "https://github.com/openai/codex/releases/download/rust-v$version/codex-package-$platform.tar.gz"
 echo "$checksum  $archive" | sha256sum -c -
 tar -xzf "$archive" -C "$destination"
fi
