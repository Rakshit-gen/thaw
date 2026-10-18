#!/bin/bash
# Builds the guest root filesystem: Firecracker's Ubuntu 24.04 CI image
# plus what the three benchmark environments need. Run as root inside the
# Linux host (the Lima VM). Takes ten minutes or so, mostly apt and git.
#
#   sudo scripts/build-rootfs.sh ~/fc/ubuntu.squashfs /opt/thaw/rootfs.ext4
set -euo pipefail

SRC=${1:?squashfs image}
OUT=${2:?output ext4 image}
SIZE=${SIZE:-4G}
WORK=$(mktemp -d /tmp/thaw-rootfs.XXXX)
ROOT=$WORK/root

cleanup() {
	for m in dev/pts dev proc sys; do umount "$ROOT/$m" 2>/dev/null || true; done
}
trap cleanup EXIT

unsquashfs -q -d "$ROOT" "$SRC"
# The CI image is trimmed: put back what apt expects to exist.
mkdir -p "$ROOT/tmp" "$ROOT/var/cache/apt/archives/partial" "$ROOT/var/lib/apt/lists/partial" "$ROOT/var/log/apt"
chmod 1777 "$ROOT/tmp"
for m in proc sys dev dev/pts; do mkdir -p "$ROOT/$m"; mount --bind "/$m" "$ROOT/$m"; done
rm -f "$ROOT/etc/resolv.conf"
cp /etc/resolv.conf "$ROOT/etc/resolv.conf"

chroot "$ROOT" /bin/bash -euxo pipefail <<'IN'
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q --no-install-recommends \
	python3 python3-numpy python3-pandas python3-asgiref python3-sqlparse \
	git ca-certificates nodejs npm
npm install -g --no-fund --no-audit typescript@5.6.3
mkdir -p /work
git clone -q --depth 1 --branch 5.1.3 https://github.com/django/django /work/django
git clone -q --depth 1 --branch v5.6.3 --filter=blob:none --sparse https://github.com/microsoft/TypeScript /work/typescript
cd /work/typescript && git sparse-checkout set src/compiler src/lib
apt-get clean
rm -rf /var/lib/apt/lists/* /root/.npm /usr/share/doc /usr/share/man
IN

cleanup
trap - EXIT
rm -f "$OUT"
truncate -s "$SIZE" "$OUT"
mkfs.ext4 -q -d "$ROOT" -L thaw "$OUT"
rm -rf "$WORK"
echo "built $OUT"
