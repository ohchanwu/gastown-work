#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BUILD_DIR="$(mktemp -d)"
trap 'rm -rf "$BUILD_DIR"' EXIT

CGO_ENABLED=0 make -C "$REPO_ROOT" BUILD_DIR="$BUILD_DIR" build >/dev/null

module_metadata="$(go version -m "$BUILD_DIR/gt")"
if grep -qE $'^[[:space:]]*build[[:space:]]+vcs\.(revision|modified)=' <<<"$module_metadata"; then
	echo "FAIL: official build contains automatic Go VCS metadata" >&2
	grep -E $'^[[:space:]]*build[[:space:]]+vcs\.(revision|modified)=' <<<"$module_metadata" >&2
	exit 1
fi

expected_commit="$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
version_output="$(GT_TEST_DOLT_PORT="${GT_TEST_DOLT_PORT:-33429}" "$BUILD_DIR/gt" version --verbose)"
if ! grep -q "@$expected_commit" <<<"$version_output"; then
	echo "FAIL: gt version --verbose does not contain explicit commit @$expected_commit" >&2
	echo "$version_output" >&2
	exit 1
fi

echo "PASS: official build uses only the explicit Gas Town commit label @$expected_commit"
