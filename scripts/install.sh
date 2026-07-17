#!/bin/sh
set -eu

APP=${MACAZ_BINARY_NAME:-macaz}
REPO=${MACAZ_REPO:-macaz-dev/macaz-cli}
INSTALL_DIR=${MACAZ_INSTALL_DIR:-${HOME:?}/.local/bin}
REQUESTED_VERSION=${MACAZ_VERSION:-${VERSION:-}}
DOWNLOAD_BASE_URL=${MACAZ_DOWNLOAD_BASE_URL:-}
LOCAL_BINARY_PATH=
NO_MODIFY_PATH=false
TEMP_DIR=
STAGED_PATH=

usage() {
	cat <<'EOF'
macaz installer

Usage: install.sh [options]

Options:
  -h, --help                Display this help
  -v, --version <version>   Install a release such as 0.1.0 or v0.1.0
  -b, --binary <path>       Install a local binary instead of downloading
  -d, --install-dir <path>  Override the installation directory
      --no-modify-path      Do not update an existing shell configuration

Environment:
  MACAZ_VERSION             Requested release version
  MACAZ_INSTALL_DIR         Installation directory (default: ~/.local/bin)
  MACAZ_REPO                GitHub repository in owner/name form
  MACAZ_BINARY_NAME         Installed binary name
  MACAZ_DOWNLOAD_BASE_URL   Asset base URL override, for mirrors and testing

Examples:
  curl -fsSL https://raw.githubusercontent.com/macaz-dev/macaz-cli/main/scripts/install.sh | sh
  curl -fsSL https://raw.githubusercontent.com/macaz-dev/macaz-cli/main/scripts/install.sh | sh -s -- --version 0.1.0
  ./scripts/install.sh --binary ./macaz --install-dir /tmp/macaz-bin
EOF
}

fail() {
	printf '%s\n' "macaz install failed: $*" >&2
	exit 1
}

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

download() {
	url=$1
	output=$2
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --retry-delay 1 "$url" -o "$output"
	elif command -v wget >/dev/null 2>&1; then
		wget -q "$url" -O "$output"
	else
		fail "curl or wget is required"
	fi
}

cleanup() {
	if [ -n "$STAGED_PATH" ] && [ -e "$STAGED_PATH" ]; then
		rm -f "$STAGED_PATH"
	fi
	if [ -n "$TEMP_DIR" ] && [ -d "$TEMP_DIR" ]; then
		rm -rf "$TEMP_DIR"
	fi
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	-v | --version)
		[ "$#" -ge 2 ] || fail "$1 requires a version"
		REQUESTED_VERSION=$2
		shift 2
		;;
	-b | --binary)
		[ "$#" -ge 2 ] || fail "$1 requires a path"
		LOCAL_BINARY_PATH=$2
		shift 2
		;;
	-d | --install-dir)
		[ "$#" -ge 2 ] || fail "$1 requires a path"
		INSTALL_DIR=$2
		shift 2
		;;
	--no-modify-path)
		NO_MODIFY_PATH=true
		shift
		;;
	*)
		fail "unknown option: $1"
		;;
	esac
done

detect_os() {
	case "$(uname -s)" in
	Darwin) printf '%s\n' darwin ;;
	Linux) printf '%s\n' linux ;;
	MINGW* | MSYS* | CYGWIN*) printf '%s\n' windows ;;
	*) fail "unsupported operating system: $(uname -s)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) printf '%s\n' amd64 ;;
	arm64 | aarch64) printf '%s\n' arm64 ;;
	*) fail "unsupported CPU architecture: $(uname -m)" ;;
	esac
}

adjust_arch_for_rosetta() {
	os=$1
	arch=$2
	if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
		translated=$(sysctl -n sysctl.proc_translated 2>/dev/null || printf '%s\n' 0)
		if [ "$translated" = 1 ]; then
			printf '%s\n' arm64
			return
		fi
	fi
	printf '%s\n' "$arch"
}

latest_version() {
	metadata="$TEMP_DIR/latest-release.json"
	download "https://api.github.com/repos/$REPO/releases/latest" "$metadata" ||
		fail "unable to query the latest release from $REPO"
	tag=$(
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
			"$metadata" | head -n 1
	)
	[ -n "$tag" ] || fail "the latest GitHub release did not contain a tag"
	printf '%s\n' "$tag"
}

valid_version() {
	printf '%s\n' "$1" |
		awk '/^[0-9]+\.[0-9]+\.[0-9]+$/ { valid = 1 } END { exit !valid }'
}

checksum() {
	file=$1
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$file" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$file" | awk '{print $1}'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$file" | awk '{print $NF}'
	else
		fail "sha256sum, shasum, or openssl is required"
	fi
}

verify_checksum() {
	artifact_path=$1
	sums_path=$2
	artifact_name=$(basename "$artifact_path")
	expected=$(
		awk -v file="$artifact_name" '
			{
				name = $2
				sub(/^\*/, "", name)
				if (name == file) {
					print $1
					exit
				}
			}
		' "$sums_path"
	)
	[ -n "$expected" ] || fail "$artifact_name is missing from SHA256SUMS"
	actual=$(checksum "$artifact_path")
	[ "$actual" = "$expected" ] || fail "checksum mismatch for $artifact_name"
	printf '%s\n' "SHA-256 verified for $artifact_name."
}

installed_version() {
	command -v "$APP" >/dev/null 2>&1 || return 0
	"$APP" version 2>/dev/null | awk 'NR == 1 { print $2 }'
}

install_binary() {
	source_path=$1
	os=$2
	destination="$INSTALL_DIR/$APP"
	if [ "$os" = windows ]; then
		destination="$INSTALL_DIR/$APP.exe"
	fi

	mkdir -p "$INSTALL_DIR" || fail "cannot create $INSTALL_DIR"
	STAGED_PATH="$INSTALL_DIR/.$APP.install.$$"
	cp "$source_path" "$STAGED_PATH" || fail "cannot stage the binary in $INSTALL_DIR"
	chmod 755 "$STAGED_PATH" || fail "cannot make the staged binary executable"
	mv -f "$STAGED_PATH" "$destination" || fail "cannot install the binary at $destination"
	STAGED_PATH=
	printf '%s\n' "macaz installed: $destination"
	"$destination" version >/dev/null || fail "the installed binary did not start correctly"
}

add_to_path() {
	config_file=$1
	path_command=$2
	if grep -Fqx "$path_command" "$config_file" 2>/dev/null; then
		return
	fi
	if [ ! -e "$config_file" ] || [ ! -w "$config_file" ]; then
		printf '%s\n' "Add this command to your shell configuration:"
		printf '  %s\n' "$path_command"
		return
	fi
	printf '\n# macaz\n%s\n' "$path_command" >> "$config_file"
	printf '%s\n' "Added $INSTALL_DIR to PATH in $config_file."
}

ensure_path() {
	case ":${PATH:-}:" in
	*":$INSTALL_DIR:"*) return ;;
	esac

	current_shell=$(basename "${SHELL:-sh}")
	case "$current_shell" in
	fish)
		add_to_path "$HOME/.config/fish/config.fish" "fish_add_path \"$INSTALL_DIR\""
		;;
	zsh)
		add_to_path "${ZDOTDIR:-$HOME}/.zshrc" "export PATH=\"$INSTALL_DIR:\$PATH\""
		;;
	bash)
		if [ -e "$HOME/.bashrc" ]; then
			config_file=$HOME/.bashrc
		else
			config_file=$HOME/.bash_profile
		fi
		add_to_path "$config_file" "export PATH=\"$INSTALL_DIR:\$PATH\""
		;;
	*)
		add_to_path "$HOME/.profile" "export PATH=\"$INSTALL_DIR:\$PATH\""
		;;
	esac
}

need_cmd uname
need_cmd awk
need_cmd sed
need_cmd mktemp
need_cmd cp
need_cmd chmod
need_cmd mkdir
need_cmd mv
need_cmd grep
need_cmd basename

OS=$(detect_os)
ARCH=$(adjust_arch_for_rosetta "$OS" "$(detect_arch)")
TEMP_DIR=$(mktemp -d 2>/dev/null || mktemp -d -t macaz-install)
trap cleanup EXIT HUP INT TERM

if [ -n "$LOCAL_BINARY_PATH" ]; then
	[ -f "$LOCAL_BINARY_PATH" ] || fail "binary not found: $LOCAL_BINARY_PATH"
	install_binary "$LOCAL_BINARY_PATH" "$OS"
else
	if [ -z "$REQUESTED_VERSION" ] || [ "$REQUESTED_VERSION" = latest ]; then
		REQUESTED_VERSION=$(latest_version)
	fi
	VERSION_NUMBER=${REQUESTED_VERSION#v}
	valid_version "$VERSION_NUMBER" || fail "invalid release version: $REQUESTED_VERSION"

	current_version=$(installed_version || true)
	if [ "$current_version" = "v$VERSION_NUMBER" ]; then
		printf '%s\n' "$APP v$VERSION_NUMBER is already installed."
		exit 0
	fi

	extension=
	if [ "$OS" = windows ]; then
		extension=.exe
	fi
	artifact="$APP-$OS-$ARCH$extension"
	base_url=${DOWNLOAD_BASE_URL:-https://github.com/$REPO/releases/download/v$VERSION_NUMBER}
	base_url=${base_url%/}

	printf '%s\n' "Downloading $artifact from $REPO release v$VERSION_NUMBER..."
	download "$base_url/$artifact" "$TEMP_DIR/$artifact" ||
		fail "unable to download $artifact"
	download "$base_url/SHA256SUMS" "$TEMP_DIR/SHA256SUMS" ||
		fail "unable to download SHA256SUMS"
	verify_checksum "$TEMP_DIR/$artifact" "$TEMP_DIR/SHA256SUMS"
	install_binary "$TEMP_DIR/$artifact" "$OS"
fi

if [ "$NO_MODIFY_PATH" != true ] && [ "$OS" != windows ]; then
	ensure_path
fi

if [ -n "${GITHUB_ACTIONS:-}" ] && [ "${GITHUB_ACTIONS:-}" = true ] &&
	[ -n "${GITHUB_PATH:-}" ]; then
	printf '%s\n' "$INSTALL_DIR" >> "$GITHUB_PATH"
fi

printf '\n%s\n' "Start with:"
printf '  %s\n' "macaz --help"
printf '  %s\n' "macaz"
