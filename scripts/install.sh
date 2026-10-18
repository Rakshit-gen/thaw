#!/bin/sh
# thaw installer.
#
#   curl -fsSL https://raw.githubusercontent.com/Rakshit-gen/thaw/main/scripts/install.sh | sh
#
# Downloads the release for your OS and architecture from GitHub Releases,
# checks its SHA256, and installs thaw (and on Linux, the thaw-guest agent
# next to it) into a user-local bin directory.
#
# Environment:
#   THAW_VERSION   version to install (default: latest)
#   THAW_BIN_DIR   install directory (default: $HOME/.local/bin)
#   THAW_REPO      owner/repo (default: Rakshit-gen/thaw)
#   THAW_DOWNLOAD_BASE  download from this URL instead of the GitHub release,
#                  for mirrors; needs THAW_VERSION

set -eu

REPO="${THAW_REPO:-Rakshit-gen/thaw}"
VERSION="${THAW_VERSION:-latest}"
BIN_DIR="${THAW_BIN_DIR:-$HOME/.local/bin}"

info() { printf '  %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -q "$1" -O "$2"
	else
		die "need curl or wget"
	fi
}

fetch_stdout() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	else
		wget -qO- "$1"
	fi
}

main() {
	command -v tar >/dev/null 2>&1 || die "need tar"

	case "$(uname -s)" in
		Linux) os=linux ;;
		Darwin) os=darwin ;;
		*) die "unsupported OS $(uname -s); thaw ships for Linux and macOS" ;;
	esac
	case "$(uname -m)" in
		x86_64 | amd64) arch=amd64 ;;
		arm64 | aarch64) arch=arm64 ;;
		*) die "unsupported architecture $(uname -m); thaw ships for amd64 and arm64" ;;
	esac

	if [ "$VERSION" = latest ]; then
		VERSION="$(fetch_stdout "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
		[ -n "$VERSION" ] || die "could not find the latest release; set THAW_VERSION"
	fi
	tag="$VERSION"
	case "$tag" in v*) ;; *) tag="v$tag" ;; esac
	archive="thaw_${tag#v}_${os}_${arch}.tar.gz"
	base="${THAW_DOWNLOAD_BASE:-https://github.com/${REPO}/releases/download/${tag}}"

	tmp="$(mktemp -d "${TMPDIR:-/tmp}/thaw.XXXXXX")"
	trap 'rm -rf "$tmp"' EXIT

	info "downloading ${archive}"
	fetch "${base}/${archive}" "${tmp}/${archive}" || die "download failed: ${base}/${archive}"
	fetch "${base}/checksums.txt" "${tmp}/checksums.txt" || die "could not download checksums.txt"

	want="$(grep " ${archive}\$" "${tmp}/checksums.txt" | awk '{print $1}')"
	[ -n "$want" ] || die "no checksum for ${archive}"
	if command -v sha256sum >/dev/null 2>&1; then
		got="$(sha256sum "${tmp}/${archive}" | awk '{print $1}')"
	else
		got="$(shasum -a 256 "${tmp}/${archive}" | awk '{print $1}')"
	fi
	[ "$want" = "$got" ] || die "checksum mismatch for ${archive}"
	info "checksum verified"

	tar -xzf "${tmp}/${archive}" -C "$tmp"
	mkdir -p "$BIN_DIR"
	for b in thaw thaw-guest; do
		if [ -f "${tmp}/${b}" ]; then
			mv "${tmp}/${b}" "${BIN_DIR}/${b}"
			chmod +x "${BIN_DIR}/${b}"
			info "installed ${BIN_DIR}/${b}"
		fi
	done
	[ -x "${BIN_DIR}/thaw" ] || die "archive had no thaw binary"

	case ":${PATH}:" in
		*":${BIN_DIR}:"*) ;;
		*)
			printf '\n'
			info "${BIN_DIR} is not on your PATH. Add it to your shell profile:"
			printf '\n    export PATH="%s:$PATH"\n\n' "$BIN_DIR"
			;;
	esac
	if [ "$os" = linux ]; then
		info "next: sudo thaw doctor"
	else
		info "on macOS thaw works with stores, plans and replays; VMs need Linux"
	fi
}

main "$@"
