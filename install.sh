#!/bin/sh
# Install codemcp on Linux from the GitHub releases of chriswirz/code-mcp.
#
#   curl -fsSL https://raw.githubusercontent.com/chriswirz/code-mcp/main/install.sh | sh
#
# The script downloads the release binary for this machine's architecture,
# checks it against the published SHA256SUMS, and installs it as `codemcp`.
#
# It installs system-wide into /usr/local/bin, mode 0755, so that `codemcp`
# works both for your own account and under root or sudo - which matters
# because a server started with sudo and one started as yourself are the two
# ways people actually run it. Only when it cannot write there (no sudo, or
# CODEMCP_NO_SUDO=1) does it fall back to ~/.local/bin, which is reachable by
# your user alone; the script says so when that happens.
#
# Environment:
#   CODEMCP_VERSION   release tag to install (default: the latest release)
#   CODEMCP_INSTALL_DIR  where to put the binary (default: as described above)
#   CODEMCP_NO_SUDO=1 never escalate; fall back to a user-local install
#
# POSIX sh on purpose: this has to run on whatever shell the machine has.
set -eu

REPO="chriswirz/code-mcp"
BINARY="codemcp"
VERSION="${CODEMCP_VERSION:-}"
INSTALL_DIR="${CODEMCP_INSTALL_DIR:-}"

log()  { printf '%s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

# fetch writes a URL to a file, using whichever downloader is present. Both are
# told to fail on an HTTP error rather than saving the error page.
fetch() {
	url="$1" out="$2"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$out" "$url"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$out" "$url"
	else
		die "neither curl nor wget is installed"
	fi
}

# target_arch maps uname's name for the machine onto the one the release assets
# are built for.
target_arch() {
	machine="$(uname -m)"
	case "$machine" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported architecture: $machine (releases are built for amd64 and arm64)" ;;
	esac
}

# choose_install_dir prefers the system-wide /usr/local/bin, since that is on
# the default PATH for root and for ordinary users alike, and only falls back
# to a user-local directory when it cannot be written.
choose_install_dir() {
	if [ -n "$INSTALL_DIR" ]; then
		echo "$INSTALL_DIR"
		return
	fi
	if [ -w /usr/local/bin ] 2>/dev/null; then
		echo /usr/local/bin
		return
	fi
	if [ "${CODEMCP_NO_SUDO:-}" != "1" ] && [ "$(id -u)" -ne 0 ] &&
		command -v sudo >/dev/null 2>&1; then
		echo /usr/local/bin
		return
	fi
	if [ "$(id -u)" -eq 0 ]; then
		echo /usr/local/bin
		return
	fi
	echo "$HOME/.local/bin"
}

# install_file puts the downloaded binary in place, escalating only if the
# destination is not writable as the current user.
install_file() {
	src="$1" dest_dir="$2" dest="$2/$BINARY"
	if [ -d "$dest_dir" ] && [ -w "$dest_dir" ]; then
		mv -f "$src" "$dest"
		chmod 0755 "$dest"
		return
	fi
	if [ ! -d "$dest_dir" ] && mkdir -p "$dest_dir" 2>/dev/null; then
		mv -f "$src" "$dest"
		chmod 0755 "$dest"
		return
	fi
	[ "${CODEMCP_NO_SUDO:-}" = "1" ] && die "$dest_dir is not writable and sudo is disabled"
	command -v sudo >/dev/null 2>&1 || die "$dest_dir is not writable and sudo is not installed"
	log "Escalating with sudo to write to $dest_dir"
	sudo mkdir -p "$dest_dir"
	# Owned by root and mode 0755: readable and executable by every account,
	# writable by none but root.
	sudo install -o root -g root -m 0755 "$src" "$dest" ||
		sudo install -m 0755 "$src" "$dest"
	rm -f "$src"
}

# verify_checksum checks the download against SHA256SUMS. A missing sha256 tool
# is a warning rather than a failure: the download came over TLS either way.
verify_checksum() {
	file="$1" name="$2" sums="$3"
	if command -v sha256sum >/dev/null 2>&1; then
		actual="$(sha256sum "$file" | cut -d' ' -f1)"
	elif command -v shasum >/dev/null 2>&1; then
		actual="$(shasum -a 256 "$file" | cut -d' ' -f1)"
	else
		warn "no sha256sum or shasum available; skipping checksum verification"
		return
	fi
	expected="$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }' "$sums" | head -n 1)"
	if [ -z "$expected" ]; then
		warn "$name is not listed in SHA256SUMS; skipping checksum verification"
		return
	fi
	[ "$actual" = "$expected" ] ||
		die "checksum mismatch for $name (expected $expected, got $actual)"
	log "Checksum verified."
}

# report_reach says who can run what was just installed. A system-wide install
# serves both your account and root; a user-local one does not, and the way to
# fix that is worth printing rather than leaving to be discovered when a sudo
# run says "command not found".
report_reach() {
	dir="$1" installed="$2"
	case "$dir" in
	/usr/local/bin | /usr/bin | /bin | /opt/*)
		log ""
		log "Usable as your user and as root (mode 0755 in $dir)."
		if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
			if ! sudo -n true 2>/dev/null; then
				log "Under sudo, run it as: sudo $installed"
			fi
		fi
		;;
	*)
		log ""
		log "Installed for your user only: $dir is not on root's PATH, and root may"
		log "not be able to read it at all if your home directory is private."
		log "To make it work as both your user and root:"
		log "  sudo install -o root -g root -m 0755 $installed /usr/local/bin/codemcp"
		;;
	esac
}

main() {
	[ "$(uname -s)" = "Linux" ] || die "this installer is for Linux; got $(uname -s)"
	need uname
	need awk

	arch="$(target_arch)"
	asset="${BINARY}-linux-${arch}"

	if [ -n "$VERSION" ]; then
		base="https://github.com/$REPO/releases/download/$VERSION"
		log "Installing $BINARY $VERSION for linux/$arch"
	else
		base="https://github.com/$REPO/releases/latest/download"
		log "Installing the latest $BINARY release for linux/$arch"
	fi

	tmp="$(mktemp -d)"
	# shellcheck disable=SC2064 # expand tmp now, not at trap time
	trap "rm -rf '$tmp'" EXIT INT TERM

	log "Downloading $base/$asset"
	fetch "$base/$asset" "$tmp/$asset" ||
		die "could not download $asset (check the release exists at https://github.com/$REPO/releases)"

	if fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null; then
		verify_checksum "$tmp/$asset" "$asset" "$tmp/SHA256SUMS"
	else
		warn "SHA256SUMS could not be downloaded; skipping checksum verification"
	fi

	chmod +x "$tmp/$asset"
	dir="$(choose_install_dir)"
	install_file "$tmp/$asset" "$dir"

	installed="$dir/$BINARY"
	log ""
	log "Installed $installed"
	if version_line="$("$installed" --version 2>/dev/null)"; then
		log "  $version_line"
	fi

	case ":$PATH:" in
	*":$dir:"*) ;;
	*)
		log ""
		log "$dir is not on your PATH. Add it with:"
		log "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile && . ~/.profile"
		;;
	esac

	report_reach "$dir" "$installed"

	log ""
	log "Run it in a project directory:"
	log "  cd /path/to/project && $BINARY"
}

main "$@"
