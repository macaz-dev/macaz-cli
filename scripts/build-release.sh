#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
cd "$ROOT"

VERSION=${1:-${VERSION:-}}
OUTPUT_DIR=${OUTPUT_DIR:-"$ROOT/dist"}

case "$VERSION" in
	v[0-9]*) ;;
	*)
		printf '%s\n' "Usage: ./scripts/build-release.sh vMAJOR.MINOR.PATCH" >&2
		printf '%s\n' "Production VERSION must start with v followed by a number." >&2
		exit 1
		;;
esac

mkdir -p "$OUTPUT_DIR"
for existing in "$OUTPUT_DIR"/* "$OUTPUT_DIR"/.[!.]* "$OUTPUT_DIR"/..?*; do
	if [ -e "$existing" ]; then
		printf '%s\n' "Production build refused: output directory is not empty: $OUTPUT_DIR" >&2
		printf '%s\n' "Use a new empty OUTPUT_DIR to prevent stale release artifacts." >&2
		exit 1
	fi
done

build_target() {
	target_goos=$1
	target_goarch=$2
	extension=
	if [ "$target_goos" = "windows" ]; then
		extension=.exe
	fi
	output="$OUTPUT_DIR/macaz-$target_goos-$target_goarch$extension"
	CGO_ENABLED=0 GOOS="$target_goos" GOARCH="$target_goarch" \
		go build -buildvcs=false -trimpath \
		-ldflags "-s -w -X=main.version=$VERSION" \
		-o "$output" \
		./cmd/macaz
	printf '%s\n' "Built: $output"
}

if [ -n "${GOOS:-}" ] || [ -n "${GOARCH:-}" ]; then
	if [ -z "${GOOS:-}" ] || [ -z "${GOARCH:-}" ]; then
		printf '%s\n' "GOOS and GOARCH must be provided together." >&2
		exit 1
	fi
	build_target "$GOOS" "$GOARCH"
else
	for target in \
		"darwin amd64" \
		"darwin arm64" \
		"linux amd64" \
		"linux arm64" \
		"windows amd64" \
		"windows arm64"
	do
		set -- $target
		build_target "$1" "$2"
	done
fi

cp "$ROOT/README.md" "$OUTPUT_DIR/README.md"
cp "$ROOT/scripts/install.sh" "$OUTPUT_DIR/install.sh"
chmod 755 "$OUTPUT_DIR/install.sh"
cp "$ROOT/THIRD_PARTY_NOTICES.md" "$OUTPUT_DIR/THIRD_PARTY_NOTICES.md"
cp "$ROOT/LICENSE" "$OUTPUT_DIR/LICENSE"
cp "$ROOT/NOTICE" "$OUTPUT_DIR/NOTICE"
cp "$ROOT/LEGAL.md" "$OUTPUT_DIR/LEGAL.md"
cp "$ROOT/PRIVACY.md" "$OUTPUT_DIR/PRIVACY.md"
(
	cd "$OUTPUT_DIR"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum macaz-* > SHA256SUMS
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 macaz-* > SHA256SUMS
	else
		printf '%s\n' "Neither sha256sum nor shasum is available." >&2
		exit 1
	fi
)

printf '%s\n' "Version: $VERSION"
printf '%s\n' "Production directory: $OUTPUT_DIR"
printf '%s\n' "Public documentation and checksums were added."
printf '%s\n' "Access: free and unrestricted under Apache-2.0."
