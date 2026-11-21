#!/bin/sh
# Installs the latest (or RW_VERSION) runner-warden release on Linux:
#   curl -fsSL https://raw.githubusercontent.com/kylepantall/runner-warden/main/packaging/install.sh | sh
# Then run: sudo runner-warden setup
set -eu

REPO=kylepantall/runner-warden
BIN_DIR=${RW_BIN_DIR:-/usr/local/bin}
UNIT_DIR=${RW_UNIT_DIR:-/etc/systemd/system}

fail() { echo "install: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"; }

need curl
need tar
need sha256sum
[ "$(uname -s)" = Linux ] || fail "this script installs the Linux build; on macOS download the darwin archive from the releases page"

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

VERSION=${RW_VERSION:-}
if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n1)
  [ -n "$VERSION" ] || fail "could not determine the latest release"
fi
VERSION=${VERSION#v}

SUDO=""
[ "$(id -u)" -eq 0 ] || SUDO=sudo

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
ARCHIVE="runner-warden_${VERSION}_linux_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/v$VERSION"

echo "Downloading runner-warden $VERSION ($ARCH)"
curl -fsSL -o "$TMP/$ARCHIVE" "$BASE/$ARCHIVE"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt"
# Authenticity: checksums.txt is signed keylessly by the release workflow.
if command -v cosign >/dev/null 2>&1; then
  curl -fsSL -o "$TMP/checksums.txt.pem" "$BASE/checksums.txt.pem"
  curl -fsSL -o "$TMP/checksums.txt.sig" "$BASE/checksums.txt.sig"
  cosign verify-blob \
    --certificate "$TMP/checksums.txt.pem" --signature "$TMP/checksums.txt.sig" \
    --certificate-identity-regexp "^https://github.com/$REPO/\.github/workflows/release\.yml@refs/tags/v" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$TMP/checksums.txt" >/dev/null 2>&1 || fail "signature verification failed"
  echo "Signature verified"
elif [ "${RW_REQUIRE_SIGNATURE:-0}" = 1 ]; then
  fail "cosign not installed and RW_REQUIRE_SIGNATURE=1"
else
  echo "warning: cosign not installed; checksum verified but signature not checked" >&2
fi
(cd "$TMP" && grep " $ARCHIVE\$" checksums.txt | sha256sum -c -) || fail "checksum mismatch"

tar -xzf "$TMP/$ARCHIVE" -C "$TMP"
$SUDO install -m 0755 "$TMP/runner-warden" "$BIN_DIR/runner-warden"
$SUDO install -m 0644 "$TMP/packaging/systemd/runner-warden@.service" "$UNIT_DIR/runner-warden@.service"
$SUDO systemctl daemon-reload

echo "Installed $BIN_DIR/runner-warden $VERSION"
echo "Next: sudo runner-warden setup"
