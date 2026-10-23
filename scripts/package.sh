#!/bin/bash
# Build and pack the release archives of one version, one per platform:
#
#   tg-<version>-macos-arm64.tar.gz     tg-<version>-macos-x86_64.tar.gz
#   tg-<version>-linux-arm64.tar.gz     tg-<version>-linux-x86_64.tar.gz
#
# Each holds tg (0755), LICENSE and README.md at top level and comes with a
# <file>.sha256 sidecar in `shasum -a 256` format (`<hex>  <file>`), so
# `shasum -a 256 -c <file>.sha256` (or `sha256sum -c`) verifies a download.
#
#   scripts/package.sh <version> <outdir>
#
# Environment:
#   SIGN_IDENTITY   a Developer ID Application identity. On macOS each darwin
#                   binary is signed with it (hardened runtime, timestamp).
#                   Empty: no signing step.
#   NOTARY_PROFILE  a notarytool keychain profile. With SIGN_IDENTITY, each
#                   signed binary goes through scripts/notarize.sh, which also
#                   launches it once, quarantined, when the host can execute
#                   it. Release builds are expected to come from an Apple
#                   Silicon Mac with Rosetta installed, where both darwin
#                   binaries get that launch; without Rosetta, or on an Intel
#                   Mac for the arm64 build, notarize.sh skips the launch and
#                   passes on Apple's acceptance, codesign --check-notarization
#                   and spctl alone (it says so in its output).
#   SOURCE_DATE_EPOCH
#                   mtime of the archive entries (default: the commit date of
#                   HEAD, or now outside git). The archives are reproducible:
#                   fixed entry order and mtime, uid/gid 0, no owner names,
#                   no extended attributes, no gzip timestamp.
#
# `make dist` calls this with the version from `git describe`. The Go
# toolchain cross-compiles everything (CGO_ENABLED=0), so the four archives
# build on any host; only signing and notarization need macOS.
set -euo pipefail

usage="usage: scripts/package.sh <version> <outdir>"
VERSION="${1:?$usage}"
OUTDIR_ARG="${2:?$usage}"
SIGN_IDENTITY="${SIGN_IDENTITY:-}"
NOTARY_PROFILE="${NOTARY_PROFILE:-}"

fail() { echo "package: $*" >&2; exit 1; }
step() { echo "==> $*"; }

ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$ROOT"
[ -f LICENSE ] || fail "LICENSE is missing in $ROOT"
[ -f README.md ] || fail "README.md is missing in $ROOT"
[ -f cmd/tg/main.go ] || fail "cmd/tg/main.go is missing in $ROOT"
case "$VERSION" in
  ""|*/*|*" "*|-*) fail "bad version: '$VERSION'" ;;
esac

host_os="$(uname -s)"
if [ -n "$NOTARY_PROFILE" ] && [ -z "$SIGN_IDENTITY" ]; then
  fail "NOTARY_PROFILE needs SIGN_IDENTITY (an unsigned binary cannot be notarized)"
fi
if [ -n "$SIGN_IDENTITY" ] && [ "$host_os" != Darwin ]; then
  fail "signing needs macOS (codesign); unset SIGN_IDENTITY for an unsigned build"
fi

# Entry mtime: SOURCE_DATE_EPOCH, else the commit date of HEAD, else now.
epoch="${SOURCE_DATE_EPOCH:-}"
if [ -z "$epoch" ]; then epoch="$(git log -1 --format=%ct 2>/dev/null || true)"; fi
if [ -z "$epoch" ]; then epoch="$(date +%s)"; fi
case "$epoch" in *[!0-9]*|"") fail "SOURCE_DATE_EPOCH must be a Unix timestamp, got '$epoch'" ;; esac
# `touch -t` wants [[CC]YY]MMDDhhmm[.SS] in local time: format in UTC, apply in UTC.
if date -u -r "$epoch" +%Y >/dev/null 2>&1; then
  stamp="$(date -u -r "$epoch" +%Y%m%d%H%M.%S)"    # BSD date (macOS)
else
  stamp="$(date -u -d "@$epoch" +%Y%m%d%H%M.%S)"   # GNU date
fi

# Deterministic tar: ustar, numeric uid/gid 0 and no owner names, no
# extended attributes, ACLs, flags or macOS resource forks. Entries are
# listed explicitly in a fixed order, so no --sort is needed.
tar_flavor="$(tar --version 2>/dev/null | head -1 || true)"
case "$tar_flavor" in
  *"GNU tar"*)
    TAR_FLAGS=(--format=ustar --owner=0 --group=0 --numeric-owner --no-xattrs) ;;
  *bsdtar*|*libarchive*)
    TAR_FLAGS=(--format ustar --uid 0 --gid 0 --numeric-owner --no-xattrs --no-acls --no-fflags)
    if [ "$host_os" = Darwin ]; then TAR_FLAGS+=(--no-mac-metadata); fi ;;
  *)
    fail "unrecognised tar (${tar_flavor:-no --version}); GNU tar or bsdtar is needed" ;;
esac

if command -v shasum >/dev/null 2>&1; then
  SHA=(shasum -a 256)
elif command -v sha256sum >/dev/null 2>&1; then
  SHA=(sha256sum)
else
  fail "neither shasum nor sha256sum is on PATH"
fi

mkdir -p "$OUTDIR_ARG"
OUTDIR="$(cd "$OUTDIR_ARG" && pwd -P)"
# The scratch directory. mktemp's answer is checked before the trap exists:
# an empty or relative WORK must never reach `rm -rf` (set -e alone would not
# catch a successful-looking but useless answer).
scratch_dir() {
  local d
  d="$(mktemp -d)" || return 1
  case "$d" in /?*) [ -d "$d" ] || return 1 ;; *) return 1 ;; esac
  d="$(cd "$d" && pwd -P)" || return 1
  case "$d" in /?*) [ -d "$d" ] || return 1 ;; *) return 1 ;; esac
  printf '%s\n' "$d"
}
WORK="$(scratch_dir)" || fail "could not create a scratch directory (mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
host_goos="$(go env GOOS)"
host_goarch="$(go env GOARCH)"

# "<GOOS>/<GOARCH>:<archive suffix>"
TARGETS="darwin/arm64:macos-arm64 darwin/amd64:macos-x86_64 linux/arm64:linux-arm64 linux/amd64:linux-x86_64"
for t in $TARGETS; do
  goos="${t%%/*}"
  rest="${t#*/}"
  goarch="${rest%%:*}"
  suffix="${rest#*:}"
  name="tg-$VERSION-$suffix"
  stage="$WORK/$name"
  mkdir -p "$stage"

  step "build $name ($goos/$goarch)"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$stage/tg" ./cmd/tg

  if [ "$goos" = darwin ] && [ -n "$SIGN_IDENTITY" ]; then
    step "sign $name"
    codesign --force --timestamp --options runtime -s "$SIGN_IDENTITY" "$stage/tg"
    if [ -n "$NOTARY_PROFILE" ]; then
      step "notarize $name"
      scripts/notarize.sh "$stage/tg" "$NOTARY_PROFILE" "$name"
    fi
  fi

  # The native build must report the version it was built with.
  if [ "$goos" = "$host_goos" ] && [ "$goarch" = "$host_goarch" ]; then
    got="$("$stage/tg" --version)"
    [ "$got" = "tg $VERSION" ] || fail "$name prints '$got', expected 'tg $VERSION'"
  fi

  cp LICENSE README.md "$stage/"
  chmod 0755 "$stage/tg"
  chmod 0644 "$stage/LICENSE" "$stage/README.md"
  TZ=UTC touch -t "$stamp" "$stage/LICENSE" "$stage/README.md" "$stage/tg"

  archive="$OUTDIR/$name.tar.gz"
  rm -f "$archive" "$archive.sha256"
  (cd "$stage" && tar "${TAR_FLAGS[@]}" -cf - LICENSE README.md tg) | gzip -n -9 > "$archive"
  (cd "$OUTDIR" && "${SHA[@]}" "$name.tar.gz" > "$name.tar.gz.sha256")
done

step "dist: $OUTDIR"
(cd "$OUTDIR" && ls -l tg-"$VERSION"-*.tar.gz && cat tg-"$VERSION"-*.tar.gz.sha256)
