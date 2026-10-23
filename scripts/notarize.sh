#!/bin/bash
# Notarize a Developer ID-signed Mach-O binary with Apple's notary service and
# check that Gatekeeper accepts it. A bare executable cannot be stapled, so
# Gatekeeper fetches the ticket online the first time a user runs it.
#
# Usage: scripts/notarize.sh <binary> <notarytool keychain profile> <submission name>
# One-time setup of the profile: xcrun notarytool store-credentials <profile> --apple-id <id> --team-id <team>
#
# The last step launches the quarantined binary once, and only when this host
# can execute it: a slice matching `uname -m`, or an x86_64 slice on Apple
# Silicon with Rosetta. Otherwise the launch is skipped, announced as such,
# and the result rests on Apple's acceptance, codesign --check-notarization
# and spctl, which run in every case.
set -euo pipefail
BIN="$1"
PROFILE="$2"
NAME="$3"
fail() { echo "notarize: $*" >&2; exit 1; }

codesign --verify --strict "$BIN"
# Captured first: with pipefail, `codesign | grep -q` fails when grep exits early.
INFO="$(codesign -dvv "$BIN" 2>&1)"
if ! grep -q "Authority=Developer ID Application" <<<"$INFO"; then
  echo "notarize: $BIN is not signed with a Developer ID Application identity" >&2
  exit 1
fi
if ! grep -q "flags=.*runtime" <<<"$INFO"; then
  echo "notarize: $BIN lacks the hardened runtime (codesign --options runtime)" >&2
  exit 1
fi

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
ZIP="$WORK/$NAME.zip"
ditto -c -k "$BIN" "$ZIP"

RESULT="$(xcrun notarytool submit "$ZIP" --keychain-profile "$PROFILE" --wait --output-format json)"
STATUS="$(/usr/bin/plutil -extract status raw -o - - <<<"$RESULT")"
ID="$(/usr/bin/plutil -extract id raw -o - - <<<"$RESULT")"
if [ "$STATUS" != "Accepted" ]; then
  echo "notarize: Apple answered $STATUS for submission $ID. Log:" >&2
  xcrun notarytool log "$ID" --keychain-profile "$PROFILE" >&2 || true
  exit 1
fi

# Apple's documented check for non-app code: the ticket must be findable.
codesign -vvvv -R="notarized" --check-notarization "$BIN" >/dev/null 2>&1 || {
  echo "notarize: accepted by Apple ($ID) but codesign --check-notarization fails for $BIN" >&2
  exit 1
}

# Gatekeeper's verdict. `--type execute` only judges app bundles ("does not
# seem to be an app" for a bare binary); `--type install` assesses the file.
if ! VERDICT="$(spctl --assess --type install -v "$BIN" 2>&1)" || ! grep -q "source=Notarized Developer ID" <<<"$VERDICT"; then
  echo "notarize: accepted by Apple ($ID) but Gatekeeper says: $VERDICT" >&2
  exit 1
fi

# The user's path: a downloaded (quarantined) binary must start without any
# xattr dance; Gatekeeper fetches the ticket online here. Only a binary this
# host can execute is launched: its own architecture, or x86_64 on Apple
# Silicon when Rosetta is present (its daemon runs, or an x86_64 slice of a
# system binary starts). Anything else is reported and skipped.
host_arch="$(uname -m)"
bin_archs="$(lipo -archs "$BIN")"
rosetta() { /usr/bin/pgrep -q oahd || /usr/bin/arch -x86_64 /usr/bin/true 2>/dev/null; }
can_run=0
for arch in $bin_archs; do
  if [ "$arch" = "$host_arch" ]; then can_run=1; fi
  if [ "$arch" = x86_64 ] && [ "$host_arch" = arm64 ] && rosetta; then can_run=1; fi
done
if [ "$can_run" = 1 ]; then
  xattr -w com.apple.quarantine "0083;$(printf '%x' "$(date +%s)");notarize.sh;" "$BIN"
  if ! OUT="$(perl -e 'alarm 60; exec @ARGV' "$BIN" version 2>&1)"; then
    echo "notarize: the quarantined binary did not start: $OUT" >&2
    xattr -d com.apple.quarantine "$BIN" || true
    exit 1
  fi
  xattr -d com.apple.quarantine "$BIN"
  LAUNCH="quarantined launch printed: $OUT"
else
  LAUNCH="launch check skipped: $bin_archs binary on $host_arch (not executable here; Apple's acceptance, codesign --check-notarization and spctl stand)"
  echo "notarize: $LAUNCH" >&2
fi
echo "notarized: submission $ID, Gatekeeper: $(grep -o 'source=.*' <<<"$VERDICT"), $LAUNCH"
