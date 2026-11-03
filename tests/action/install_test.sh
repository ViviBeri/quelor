#!/usr/bin/env bash
# Exercises the Action's install step against a fake curl, so the download and
# checksum path runs without the network. Usage: install_test.sh <install-script>
# Every case is run with a fake `go` on PATH; reaching it means the step fell
# back to building from source, which must never happen.
set -u
script="${1:?usage: install_test.sh <install-script>}"
script="$(cd "$(dirname "$script")" && pwd)/$(basename "$script")"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
fail=0

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

mkdir -p "$root/bin" "$root/good" "$root/work"
cat > "$root/bin/curl" <<'CURL'
#!/usr/bin/env bash
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift 2;; -*) shift;; *) url="$1"; shift;; esac
done
src="$FIXTURES/$(basename "$url")"
echo "$url" >> "$FIXTURES/../urls.log"
[ -f "$src" ] || exit 22
cp "$src" "$out"
CURL
cat > "$root/bin/go" <<'GO'
#!/usr/bin/env bash
echo called >> "$FIXTURES/../go-called"
exit 0
GO
chmod +x "$root/bin/curl" "$root/bin/go"

# A genuine release archive and its checksums.
mkdir -p "$root/stage"
printf '#!/bin/sh\necho genuine\n' > "$root/stage/quelor"
chmod +x "$root/stage/quelor"
tar -czf "$root/good/quelor_linux_amd64.tar.gz" -C "$root/stage" quelor
good_sum="$(sha "$root/good/quelor_linux_amd64.tar.gz")"
printf '%s  quelor_linux_amd64.tar.gz\n%s  quelor_linux_arm64.tar.gz\n' "$good_sum" "$good_sum" > "$root/good/checksums.txt"

run_case() { # name want(0|nonzero) version fixtures-dir
  local name="$1" want="$2" version="$3" fx="$4"
  rm -rf "$root/work" "$root/go-called" "$root/urls.log"; mkdir -p "$root/work"
  : > "$root/work/path"
  env PATH="$root/bin:$PATH" FIXTURES="$fx" QUELOR_VERSION="$version" \
    RUNNER_OS_NAME=Linux RUNNER_ARCH_NAME=X64 RUNNER_TEMP="$root/work" \
    GITHUB_PATH="$root/work/path" bash "$script" >"$root/out.log" 2>&1
  local code=$?
  local ok=1
  if [ "$want" = 0 ] && [ "$code" != 0 ]; then ok=0; fi
  if [ "$want" = nonzero ] && [ "$code" = 0 ]; then ok=0; fi
  if [ -f "$root/go-called" ]; then ok=0; echo "  fell back to a source build"; fi
  if [ "$want" = nonzero ] && [ -x "$root/work/quelor-bin/quelor" ]; then ok=0; echo "  a binary was installed despite the failure"; fi
  if [ "$want" = 0 ] && [ ! -x "$root/work/quelor-bin/quelor" ]; then ok=0; echo "  no binary installed"; fi
  if [ "$ok" = 1 ]; then echo "PASS $name"; else echo "FAIL $name (exit $code)"; sed 's/^/  | /' "$root/out.log" | head -5; fail=1; fi
  mkdir -p "$root/logs"
}

fx() { rm -rf "$root/$1"; cp -R "$root/good" "$root/$1"; echo "$root/$1"; }

run_case "genuine release installs" 0 v0.1.1 "$root/good"

d="$(fx tampered)"
printf '#!/bin/sh\necho evil\n' > "$root/stage/quelor"
tar -czf "$d/quelor_linux_amd64.tar.gz" -C "$root/stage" quelor
run_case "altered archive is refused" nonzero v0.1.1 "$d"

d="$(fx nosums)"; rm "$d/checksums.txt"
run_case "missing checksums.txt is refused" nonzero v0.1.1 "$d"

d="$(fx noentry)"; printf '%s  quelor_darwin_arm64.tar.gz\n' "$good_sum" > "$d/checksums.txt"
run_case "archive absent from checksums is refused" nonzero v0.1.1 "$d"

d="$(fx nodl)"; rm "$d/quelor_linux_amd64.tar.gz"
run_case "failed download does not fall back to a source build" nonzero v0.1.1 "$d"

run_case "latest is refused" nonzero latest "$root/good"
run_case "branch name is refused" nonzero main "$root/good"

exit $fail
