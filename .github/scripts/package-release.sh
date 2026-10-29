#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo 'Usage: package-release.sh <goos> <goarch> [output-directory]' >&2
  exit 1
fi
release_os=$1
release_arch=$2
case "$release_os/$release_arch" in
  windows/amd64|darwin/amd64|darwin/arm64|linux/amd64|linux/arm64) ;;
  *) echo "Unsupported release target: $release_os/$release_arch" >&2; exit 1 ;;
esac

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_dir"
mkdir -p -- "${3:-dist/releases}"
output_dir=$(cd -- "${3:-dist/releases}" && pwd)
staging_dir=$(mktemp -d)
trap 'rm -rf -- "$staging_dir"' EXIT

binary=xm
if [[ "$release_os" == windows ]]; then binary=xm.exe; fi
CGO_ENABLED=0 GOOS="$release_os" GOARCH="$release_arch" GOAMD64=v1 \
  go build -trimpath -ldflags='-s -w' -o "$staging_dir/$binary" ./cmd/xm
cp README.md "$staging_dir/"
cp -R docs "$staging_dir/"

archive_name="xm-$release_os-$release_arch"
if [[ "$release_os" == windows ]]; then
  python3 - "$staging_dir" "$output_dir/$archive_name.zip" <<'PY'
from pathlib import Path
import sys
import zipfile
root = Path(sys.argv[1])
with zipfile.ZipFile(sys.argv[2], 'w', compression=zipfile.ZIP_DEFLATED) as archive:
    for path in sorted(root.rglob('*')):
        if path.is_file():
            archive.write(path, path.relative_to(root))
PY
  echo "$output_dir/$archive_name.zip"
else
  tar -czf "$output_dir/$archive_name.tar.gz" -C "$staging_dir" .
  echo "$output_dir/$archive_name.tar.gz"
fi
