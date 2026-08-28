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
if ! grep -qE "(@|: )${expected_commit}\\)$" <<<"$version_output"; then
	echo "FAIL: gt version --verbose does not end with explicit commit $expected_commit" >&2
	echo "$version_output" >&2
	exit 1
fi

set +e
forward_output="$(make -C "$REPO_ROOT" INSTALL_DIR="$BUILD_DIR" check-forward-only 2>&1)"
forward_status=$?
set -e
if [[ $forward_status -eq 0 || "$forward_output" != *"Binary is already at HEAD, nothing to do"* ]]; then
	echo "FAIL: check-forward-only did not recognize the exact built commit" >&2
	echo "$forward_output" >&2
	exit 1
fi

echo "PASS: official build uses only the explicit Gas Town commit label @$expected_commit"
