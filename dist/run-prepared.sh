#!/usr/bin/env bash

set -euo pipefail

: "${SLIVER_EXT_CONTROLLER:?prepared controller path is required}"
: "${SLIVER_EXT_PREPARED_FILE:?prepared acquisition descriptor is required}"
: "${SLIVER_EXT_STATE_FILE:?cleanup state file is required}"
: "${SLIVER_EXT_RESULTS_DIR:?results directory is required}"
: "${SLIVER_EXT_TARGET:?target is required}"

if [[ ! -f "${SLIVER_EXT_CONTROLLER}" || ! -x "${SLIVER_EXT_CONTROLLER}" ]]; then
	echo "prepared controller is not an executable regular file: ${SLIVER_EXT_CONTROLLER}" >&2
	exit 2
fi
if [[ ! -f "${SLIVER_EXT_PREPARED_FILE}" ]]; then
	echo "prepared acquisition descriptor is not a regular file: ${SLIVER_EXT_PREPARED_FILE}" >&2
	exit 2
fi

# Replace the runner shell with a fresh environment and the compiled Go
# controller. In particular, no action input, GitHub token, Actions runtime
# token, proxy, cloud credential, or Go build control reaches Sliver or an extension.
exec env -i \
	PATH="${PATH}" \
	HOME="${HOME:-}" \
	USERPROFILE="${USERPROFILE:-}" \
	APPDATA="${APPDATA:-}" \
	LOCALAPPDATA="${LOCALAPPDATA:-}" \
	SYSTEMDRIVE="${SYSTEMDRIVE:-}" \
	SYSTEMROOT="${SYSTEMROOT:-}" \
	WINDIR="${WINDIR:-}" \
	COMSPEC="${COMSPEC:-}" \
	PATHEXT="${PATHEXT:-}" \
	TEMP="${TEMP:-}" \
	TMP="${TMP:-}" \
	TMPDIR="${TMPDIR:-}" \
	LANG="${LANG:-}" \
	LC_ALL="${LC_ALL:-}" \
	LC_CTYPE="${LC_CTYPE:-}" \
	TZ="${TZ:-}" \
	DISPLAY="${DISPLAY:-}" \
	XAUTHORITY="${XAUTHORITY:-}" \
	SSL_CERT_DIR="${SSL_CERT_DIR:-}" \
	SSL_CERT_FILE="${SSL_CERT_FILE:-}" \
	RUNNER_TEMP="${RUNNER_TEMP:-}" \
	GITHUB_WORKSPACE="${GITHUB_WORKSPACE:-}" \
	GITHUB_STEP_SUMMARY="${GITHUB_STEP_SUMMARY:-}" \
	"${SLIVER_EXT_CONTROLLER}" run \
	--manifest "${SLIVER_EXT_MANIFEST:-.github/sliver-ext-e2e.yml}" \
	--workspace "${GITHUB_WORKSPACE:-${PWD}}" \
	--library-root "${SLIVER_EXT_LIBRARY_ROOT:-}" \
	--results-dir "${SLIVER_EXT_RESULTS_DIR}" \
	--sliver-version "${SLIVER_EXT_SLIVER_VERSION:-}" \
	--target "${SLIVER_EXT_TARGET}" \
	--action-version "${SLIVER_EXT_ACTION_VERSION:-}" \
	--prepared "${SLIVER_EXT_PREPARED_FILE}" \
	--state-file "${SLIVER_EXT_STATE_FILE}"
