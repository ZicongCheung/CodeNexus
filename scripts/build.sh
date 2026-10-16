#!/usr/bin/env bash
# Build and release entry point.
#
# This script is the single source of truth for how the gateway is built: CI
# only calls it, so anything that works here works in a release and vice versa,
# and a build failure can always be reproduced on a laptop.
#
#   ./scripts/build.sh build     # this platform only -> dist/
#   ./scripts/build.sh release   # every platform + checksums -> dist/
#   ./scripts/build.sh docker    # multi-arch image (needs buildx)
#   ./scripts/build.sh icons     # regenerate the icon from the source artwork
#   ./scripts/build.sh res       # regenerate the Windows resource object
#   ./scripts/build.sh web       # rebuild the console bundle
#   ./scripts/build.sh test      # format, vet, unit tests
#   ./scripts/build.sh clean
#
# Environment:
#   VERSION        overrides the version (default: git tag, else 0.1.0-dev)
#   SKIP_WEB=1     do not rebuild the console bundle first
#   ICON_SRC       directory holding the source PNGs for `icons`
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DIST="$ROOT/dist"
BIN_NAME="codenexus"
PKG="github.com/ZicongCheung/CodeNexus"

# ---------------------------------------------------------------- utilities

say() { printf '\033[1m%s\033[0m\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# python_bin picks whichever interpreter exists. Recent Ubuntu images ship only
# `python3`, older ones only `python`, and Windows has both -- so the packaging
# steps resolve it rather than hard-coding one.
PY=""
python_bin() {
	if [ -z "$PY" ]; then
		for candidate in python3 python; do
			if command -v "$candidate" >/dev/null 2>&1; then
				PY="$candidate"
				break
			fi
		done
		[ -n "$PY" ] || die "python 3 is required for packaging"
	fi
	echo "$PY"
}

version() {
	if [ -n "${VERSION:-}" ]; then
		echo "$VERSION"
		return
	fi
	local tag
	tag="$(git describe --tags --abbrev=0 2>/dev/null || true)"
	if [ -n "$tag" ]; then
		echo "${tag#v}"
	else
		echo "0.1.0-dev"
	fi
}

# numeric_version trims a version to what the Windows version resource accepts
# (dotted digits), so a development build still stamps something valid.
numeric_version() {
	echo "$(version)" | grep -oE '^[0-9]+(\.[0-9]+)*' || echo "0.1.0"
}

# ldflags builds a statically linked, symbol-stripped binary with the version
# stamped in. $1 may add target-specific flags.
ldflags() {
	echo "-s -w -X main.version=$(version)${1:-}"
}

build_web() {
	[ "${SKIP_WEB:-}" = "1" ] && return 0
	command -v npm >/dev/null 2>&1 || die "npm is required to rebuild the console (set SKIP_WEB=1 to use the committed bundle)"
	say "building console bundle"
	(cd web && npm ci --silent && npm run build --silent)
}

# build_one <goos> <goarch> <output path> [extra ldflags]
build_one() {
	local goos="$1" goarch="$2" out="$3" extra="${4:-}"
	mkdir -p "$(dirname "$out")"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build -trimpath -ldflags "$(ldflags "$extra")" -o "$out" ./cmd/codenexus
	printf '  %-38s %s\n' "$(basename "$out")" "$(du -h "$out" | cut -f1)"
}

# ---------------------------------------------------------------- targets

cmd_build() {
	build_web
	local goos goarch out
	goos="$(go env GOOS)"
	goarch="$(go env GOARCH)"
	out="$DIST/$goos-$goarch/$BIN_NAME"
	[ "$goos" = "windows" ] && out="$out.exe"
	say "building $goos/$goarch (version $(version))"
	build_one "$goos" "$goarch" "$out"
	say "wrote $out"
}

# Windows ships twice: the windowed build is what people double-click, the
# console build is what they run when something needs diagnosing.
cmd_release() {
	build_web
	local v
	v="$(version)"
	rm -rf "$DIST"
	mkdir -p "$DIST"

	local entry
	for entry in \
		"windows amd64" \
		"windows arm64" \
		"linux amd64" \
		"linux arm64" \
		"darwin amd64" \
		"darwin arm64"; do
		set -- $entry
		local goos="$1" goarch="$2"
		local stage="$DIST/stage/$goos-$goarch"
		say "building $goos/$goarch"
		if [ "$goos" = "windows" ]; then
			# -H=windowsgui drops the console window; the console build keeps it.
			build_one "$goos" "$goarch" "$stage/CodeNexus.exe" " -H=windowsgui"
			build_one "$goos" "$goarch" "$stage/CodeNexus-console.exe"
			cp README.md LICENSE "$stage/"
			"$(python_bin)" scripts/package.py --format zip --out "$DIST/${BIN_NAME}_${v}_${goos}_${goarch}.zip" "$stage"
		else
			build_one "$goos" "$goarch" "$stage/$BIN_NAME"
			chmod +x "$stage/$BIN_NAME"
			cp README.md LICENSE "$stage/"
			# Tar has to be told which entry is the program: Windows cannot
			# record a Unix execute bit, so an undeclared binary would unpack
			# 0644 and refuse to run.
			"$(python_bin)" scripts/package.py --format tar.gz --exec "$BIN_NAME" \
				--out "$DIST/${BIN_NAME}_${v}_${goos}_${goarch}.tar.gz" "$stage"
		fi
		rm -rf "$stage"
	done
	# Each pass clears only its own leaf directory; drop the now-empty parent.
	rm -rf "$DIST/stage"

	say "checksums"
	"$(python_bin)" scripts/checksums.py "$DIST"
	ls -la "$DIST" | tail -8
}

cmd_docker() {
	local v tag="${IMAGE:-ghcr.io/zicongcheung/codenexus}"
	v="$(version)"
	command -v docker >/dev/null 2>&1 || die "docker is required"
	say "building image $tag:$v"
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg "VERSION=$v" \
		-t "$tag:$v" -t "$tag:latest" "${PUSH:+--push}" .
}

cmd_icons() {
	local src="${ICON_SRC:-}"
	[ -n "$src" ] || die "set ICON_SRC to the directory holding the source PNGs"
	"$(python_bin)" scripts/make-icons.py --src "$src" --out internal/assets
}

cmd_res() {
	command -v go-winres >/dev/null 2>&1 || die "go-winres is required: go install github.com/tc-hib/go-winres@latest"
	go-winres simply \
		--icon internal/assets/codenexus.ico \
		--out cmd/codenexus/rsrc \
		--arch amd64,arm64 \
		--product-name CodeNexus \
		--file-description "CodeNexus gateway and console" \
		--product-version "$(numeric_version)" \
		--file-version "$(numeric_version)" \
		--copyright "See LICENSE" \
		--original-filename CodeNexus.exe \
		--manifest cli
	ls -la cmd/codenexus/rsrc_windows_*.syso
}

cmd_web() { SKIP_WEB="" build_web; }

cmd_test() {
	say "gofmt"
	local unformatted
	unformatted="$(gofmt -l internal cmd 2>/dev/null || true)"
	[ -z "$unformatted" ] || die "these files need gofmt: $unformatted"
	say "go vet"
	go vet ./...
	say "go test"
	go test -count=1 ./...
	say "all checks passed"
}

cmd_clean() { rm -rf "$DIST"; say "removed $DIST"; }

case "${1:-build}" in
build) cmd_build ;;
release) cmd_release ;;
docker) cmd_docker ;;
icons) cmd_icons ;;
res) cmd_res ;;
web) cmd_web ;;
test) cmd_test ;;
clean) cmd_clean ;;
*) die "unknown target: $1" ;;
esac
