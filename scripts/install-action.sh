#!/usr/bin/env bash
# Installs the quelor binary for the GitHub Action.
# The release is pinned, its archive is checked against the release's
# checksums.txt, and any problem stops the step. There is no source-build
# fallback.
set -euo pipefail

version="${QUELOR_VERSION:?QUELOR_VERSION is required}"
os_name="${RUNNER_OS_NAME:-Linux}"
arch_name="${RUNNER_ARCH_NAME:-X64}"

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
  echo "quelor: version must be a release tag such as v0.1.1 (got '$version'); 'latest' and branch names are not accepted" >&2
  exit 1
fi

case "$os_name" in Linux) os=linux;; macOS) os=darwin;; Windows) os=windows;; *) echo "quelor: unsupported runner OS '$os_name'" >&2; exit 1;; esac
case "$arch_name" in X64) arch=amd64;; ARM64) arch=arm64;; *) echo "quelor: unsupported runner architecture '$arch_name'" >&2; exit 1;; esac

ext=tar.gz; [ "$os" = windows ] && ext=zip
archive="quelor_${os}_${arch}.${ext}"
base="https://github.com/coherera/quelor/releases/download/${version}"

dest="${RUNNER_TEMP:-/tmp}/quelor-bin"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
rm -rf "$dest"
mkdir -p "$dest"

curl -fsSL "${base}/${archive}" -o "${work}/${archive}"
curl -fsSL "${base}/checksums.txt" -o "${work}/checksums.txt"

want="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1; exit }' "${work}/checksums.txt")"
if [[ ! "$want" =~ ^[0-9a-fA-F]{64}$ ]]; then
  echo "quelor: ${archive} is not listed in checksums.txt for ${version}" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "${work}/${archive}" | awk '{print $1}')"
else
  got="$(shasum -a 256 "${work}/${archive}" | awk '{print $1}')"
fi
want="$(printf '%s' "$want" | tr 'A-F' 'a-f')"
if [ "$got" != "$want" ]; then
  echo "quelor: checksum mismatch for ${archive} (${version}); refusing to install" >&2
  exit 1
fi

if [ "$ext" = zip ]; then
  unzip -qo "${work}/${archive}" -d "$dest"
else
  tar -xzf "${work}/${archive}" -C "$dest"
fi
if [ ! -x "$dest/quelor" ] && [ ! -x "$dest/quelor.exe" ]; then
  echo "quelor: archive did not contain the quelor binary" >&2
  exit 1
fi
if [ -n "${GITHUB_PATH:-}" ]; then echo "$dest" >> "$GITHUB_PATH"; fi
echo "quelor ${version} installed and verified (sha256 ${got})"
