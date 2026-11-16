# Sliver extension end-to-end action

`sliver-test-ext` tests native [Sliver](https://github.com/BishopFox/sliver) extensions against real session and beacon implants. For each case, the harness starts a loopback Sliver server, creates a temporary operator, generates and connects an implant for the declared target, registers the native library, invokes its export, checks the final response, and tears the processes down.

This repository provides a reusable workflow for a multi-target matrix and a root action for one target in a caller-owned job. The action source and native libraries are separate: the manifest and libraries come from the caller repository. The project is available under the [MIT license](LICENSE).

## Reusable workflow

Add `.github/sliver-ext-e2e.yml` to the extension repository and call the workflow at a pinned commit:

```yaml
name: Extension end-to-end

on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  extension-e2e:
    uses: sliverarmory/sliver-test-ext/.github/workflows/e2e.yml@<full-commit-sha>
    with:
      manifest: .github/sliver-ext-e2e.yml
```

The planner validates the complete manifest, resolves the selected Sliver revision once, and creates one job per target. A `sliver-version` workflow input overrides `sliver.version` in the manifest when nonempty. Supported targets and hosted runners are:

| Target | Runner |
|---|---|
| `darwin/amd64` | `macos-15-intel` |
| `darwin/arm64` | `macos-15` |
| `linux/386` | `ubuntu-24.04` with a complete Debian i386 userspace under Docker/QEMU |
| `linux/amd64` | `ubuntu-24.04` |
| `linux/arm64` | `ubuntu-24.04-arm` |
| `windows/386` | `windows-2022` |
| `windows/amd64` | `windows-2022` |
| `windows/arm64` | `windows-11-arm` |

The manifest cannot select arbitrary runner labels. The workflow uses GitHub's `$/` self-repository action references, which bind its internal actions to the called workflow's commit. This interface requires GitHub.com and current hosted runners.

### Building libraries before planning

For source-only repositories, pass a trusted Bash command that builds **every** library referenced by the manifest, including all target architectures:

```yaml
jobs:
  extension-e2e:
    uses: sliverarmory/sliver-test-ext/.github/workflows/e2e.yml@<full-commit-sha>
    with:
      manifest: .github/sliver-ext-e2e.yml
      prepare-command: ./scripts/build-all-extensions.sh
```

The command runs once in the Ubuntu planner from the caller checkout, before manifest validation. It receives `SLIVER_EXT_E2E_PHASE=plan`; `SLIVER_EXT_E2E_TARGET`, `SLIVER_EXT_E2E_OS`, and `SLIVER_EXT_E2E_ARCH` are empty because the command builds the whole matrix. It runs under Bash error and pipeline-failure handling. Use a checked-in script for nontrivial builds. If a library requires a native target host to build, use the root action in a caller-owned matrix and build just before that step.

The planner stages only the validated manifest-referenced libraries in a private `runner.temp` tree, preserving their workspace-relative paths. Each target job and the aggregate job downloads those exact bytes into a private library root. Manifest paths and collection globs then resolve against that root. Libraries are never extracted over the caller checkout. Planning rejects symlinks, path escapes, nonportable paths, incompatible library formats and architectures, and case-insensitive destination collisions. Committed libraries follow the same staging path when `prepare-command` is omitted.

`prepare-command` is trusted caller code with workspace and network access. Its subprocess receives a limited build environment without GitHub tokens, Actions runtime tokens, OIDC variables, cloud credentials, or GitHub command-file variables. It must clean up any processes it starts. Do not run unreviewed pull-request code through `pull_request_target` or on persistent runners.

## Manifest

The [v1 JSON Schema](schema/sliver-ext-e2e-v1.schema.json) describes the strict manifest format. This example runs one Windows library in both Sliver modes and a Linux collection:

```yaml
schema: sliver-ext-e2e/v1

sliver:
  version: master

defaults:
  modes: [session, beacon]
  repeat: 1
  command_timeout: 2m

suites:
  - name: windows-amd64
    target: {os: windows, arch: amd64}
    tests:
      - name: smoke
        library: build/windows/amd64/smoke.dll
        export: Run
        init: Init
        args_utf8: hello
        expect:
          output:
            contains: [hello]

  - name: linux-arm64
    target: {os: linux, arch: arm64}
    collections:
      - name: smoke-libraries
        glob: build/linux/arm64/*.so
        export: Run
        expect:
          success_only: true
        overrides:
          special.so:
            args_base64: aGVsbG8=
            expect:
              output:
                contains: [hello]
```

Each `test` names a workspace-relative native `library` and an effective `export` to call. `init` is optional; it names the library initialization export passed to Sliver at registration. The library must be built for its suite's `os/arch` and expose functions compatible with Sliver's native extension ABI. A `collection` expands a portable `glob` into one case per matching library. Its `overrides` keys are library basenames. Tests and collections may inherit shared settings from top-level `defaults`; an override replaces only fields it names, with `expect` replacing the inherited expectation as a whole.

The harness exercises Sliver's native register, list, and call RPCs with the compiled library. It does not parse a package's `extension.json` or exercise client command registration; set the target, export, and init explicitly in this test manifest.

Use exactly one of `args_utf8` or `args_base64` for invocation bytes. `args_utf8` supplies its UTF-8 bytes directly; `args_base64` supplies decoded binary bytes. Omitting both invokes the export with empty arguments. `modes` accepts `session`, `beacon`, or both. For single-call tests, `repeat` repeats that call and records a result for each mode and repeat. `command_timeout` bounds an invocation.

### Ordered lifecycle tests

Use `tests[].steps` when several calls must run against the same registered library and live implant. Each named step has its own arguments and required `expect`; it may override the test's `export` and `command_timeout`. Put `init`, `modes`, and `repeat` on the test or defaults. For a sequence, `repeat` runs the whole step list again on that connected implant. A sequenced test cannot also set test-level arguments or expectations. The action records each step separately, so a failure identifies the call or probe that failed. Existing single-call tests keep their v1 behavior.

For a listener lifecycle, use `tcp_probe.port: auto` and pass `${TCP_PORT}` in a step's `args_utf8`. One loopback port is selected for the sequence and reused through start, stop, and restart. An `open` probe can check exact initial bytes with `banner_utf8`; a `closed` probe requires connection refusal. Each probe has a bounded `timeout`.

```yaml
tests:
  - name: vnc-lifecycle
    library: build/linux/amd64/vnc.so
    export: VNCDesktopCommand
    modes: [session, beacon]
    steps:
      - name: start
        args_utf8: 'start --port ${TCP_PORT}'
        expect: {output: {contains: [started]}}
        tcp_probe:
          port: auto
          state: open
          timeout: 10s
          banner_utf8: "RFB 003.008\n"
        command_probe:
          path: test/rfb_capture.py
          args: ['--port', '${TCP_PORT}']
          timeout: 30s
      - name: status
        args_utf8: status
        expect: {output: {contains: [running]}}
      - name: stop
        args_utf8: stop
        expect: {output: {contains: [stopped]}}
        tcp_probe: {port: auto, state: closed, timeout: 10s}
```

`command_probe` runs after the extension output assertion and TCP probe. Its `path` is a workspace-confined executable, its `args` may contain `${TCP_PORT}`, and a nonzero exit or timeout fails that step. It runs in the credential-free phase with a limited environment. A caller can use it for a full RFB handshake and framebuffer assertion; this repository's [probe script](test/fixture/rfb_probe.py) checks a deterministic synthetic framebuffer. The complete [stateful fixture manifest](test/fixture/stateful-rfb-e2e.yml) also checks duplicate start, stopped status, and restart.

`expect.output` supports `equals`, `contains`, `not_contains`, `matches` (Go regular expressions), `sha256`, and `equals_base64` for exact binary output. Use `expect.success_only: true` for an extension with no stable output. At least one explicit expectation is required. Assertions inspect the final extension response, including the completed beacon task result, rather than treating the initial asynchronous task acknowledgement as success. String equality and regular-expression matching require UTF-8 output; binary output can use `equals_base64` or `sha256`.

Library paths are relative to the caller workspace for direct action use. The reusable workflow resolves them from the planner-staged private library root. Direct use permits in-workspace library symlinks; the planner rejects them so the artifact contains only regular files. No library may resolve outside its permitted root.

## Direct action

The root action runs one target in an existing job. The caller chooses runners, builds libraries, and sets up Go:

```yaml
jobs:
  extension-e2e:
    strategy:
      fail-fast: false
      matrix:
        include:
          - {target: windows/amd64, runner: windows-2022}
          - {target: linux/arm64, runner: ubuntu-24.04-arm}
    runs-on: ${{ matrix.runner }}
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v6
        with:
          persist-credentials: false
      - uses: actions/setup-go@v6
        with:
          go-version: 1.26.6
          cache: false
      - name: Build native libraries
        run: ./scripts/build-extension.sh ${{ matrix.target }}
      - name: Test extensions
        uses: sliverarmory/sliver-test-ext@<full-commit-sha>
        with:
          manifest: .github/sliver-ext-e2e.yml
          target: ${{ matrix.target }}
```

The root action outputs `results-dir` and `resolved-sliver`. `sliver-server-path` can supply an existing server binary for a direct single-target run; its digest is recorded. For `linux/386`, use the reusable workflow unless the caller supplies a complete i386 userspace and the 32-bit compiler expected by Sliver.

This repository's CI builds [a small native extension](test/fixture/native_extension.c) and runs the root action on `linux/amd64` with [an exact output assertion](test/fixture/sliver-ext-e2e.yml) in both session and beacon modes. The fixture verifies registration, initialization, argument delivery, callback output, and the final response path.

The CI sequence job builds a second [stateful RFB extension](test/fixture/stateful_rfb_extension.c) and runs [ordered lifecycle steps](test/fixture/stateful-rfb-e2e.yml) on one implant per mode. Its host probe checks the RFB 3.8 handshake and a 2×2 raw framebuffer. The original single-call smoke job remains in CI for manifest compatibility.

For an extension that captures a real Linux display, start Xvfb in the caller job before invoking the **direct action** on a native Linux runner, then set `DISPLAY` through `GITHUB_ENV`. If Xvfb uses an authority file, set `XAUTHORITY` to that file as well. The action passes only those X11 values into the implant environment; it does not forward GitHub tokens or cloud credentials. The Linux/386 Docker route does not mount the host X11 socket or authority file, so run display capture tests on native `linux/amd64` or `linux/arm64`.

Pin a verified full commit SHA for required checks. A published release tag such as `v0.1.1` can be used for convenience.

## Sliver acquisition and lifecycle

`sliver-version` accepts `master`, `latest`, a release tag such as `v1.7.3`, or a full 40-character commit SHA. The reusable planner resolves mutable selectors once and passes one immutable selection to every target. Release assets are signature verified. Source selectors build the server in action-private Go caches. The short-lived acquisition step alone receives the optional `github-token`; the test phase executes in a fresh environment without it.

A target uses private directories under `runner.temp`. It starts `sliver-server daemon` and an HTTP listener on loopback, creates a temporary operator profile, generates the requested implant, and correlates its connection event before registration and invocation. Session calls use their final RPC response. Beacon calls wait for the matching task result and decode the completed response. Normal cleanup stops the tracked implant, listener, and server. An `always()` fallback stops only recorded process IDs and removes only recorded private paths. Operator profiles and private keys are never uploaded.

The Linux/386 route runs in a Debian i386 container with a real 32-bit loader and system libraries. Native extension code otherwise runs as the hosted runner user. These measures support repeatable tests; they do not sandbox adversarial extension code.

## Results and security

Each target writes `report.json` (`sliver-ext-e2e-results/v1`), `junit.xml`, `summary.md`, logs, and captured raw extension output. Reports include action identity, resolved Sliver provenance, target, library digest, export, mode, repeat, timing, status, and failing lifecycle stage. The reusable workflow uploads target artifacts, aggregates the exact expected case inventory, and fails on missing, duplicate, unexpected, metadata-mismatched, or failed results. The combined artifact is retained for seven days. The root action writes to `.sliver-ext-e2e/results` by default; override `results-dir` to choose another location.

Run untrusted native libraries only on ephemeral GitHub-hosted runners with `contents: read`, no unrelated secrets or cloud credentials, and checkout credentials disabled. The action's environment scrubbing prevents the explicitly forwarded GitHub token and Actions service tokens from reaching the Sliver and extension subprocess tree. Code running as the same operating-system user may still access other job resources, so keep extension E2E jobs isolated from sensitive work.
