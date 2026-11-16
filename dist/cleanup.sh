#!/usr/bin/env bash

set -euo pipefail

if [[ -z "${SLIVER_EXT_NODE:-}" || ! -x "${SLIVER_EXT_NODE}" ]]; then
	echo "::warning::Prepared cleanup runtime is unavailable; relying on the ephemeral runner teardown"
	exit 0
fi
if [[ -z "${SLIVER_EXT_ACTION_ROOT:-}" || ! -f "${SLIVER_EXT_ACTION_ROOT}/dist/post.js" ]]; then
	echo "::warning::Cleanup program is unavailable; relying on the ephemeral runner teardown"
	exit 0
fi

# The cleanup program is launched as an ordinary, explicitly scrubbed process
# rather than a JavaScript action post hook, so it never receives action inputs
# or the runner's automatic Actions service tokens.
exec env -i \
	PATH="${PATH}" \
	HOME="${HOME:-}" \
	USERPROFILE="${USERPROFILE:-}" \
	SYSTEMROOT="${SYSTEMROOT:-}" \
	WINDIR="${WINDIR:-}" \
	COMSPEC="${COMSPEC:-}" \
	PATHEXT="${PATHEXT:-}" \
	TEMP="${TEMP:-}" \
	TMP="${TMP:-}" \
	TMPDIR="${TMPDIR:-}" \
	RUNNER_TEMP="${RUNNER_TEMP:-}" \
	GITHUB_WORKSPACE="${GITHUB_WORKSPACE:-}" \
	STATE_state_file="${SLIVER_EXT_STATE_FILE:-}" \
	STATE_prepared_dir="${SLIVER_EXT_PREPARED_DIR:-}" \
	"${SLIVER_EXT_NODE}" "${SLIVER_EXT_ACTION_ROOT}/dist/post.js"
