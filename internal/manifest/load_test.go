package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadNormalizesExtensionCases(t *testing.T) {
	workspace := t.TempDir()
	library := writeELFSharedObject(t, workspace, "lib/echo.so", 2, 62)
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: master}
defaults:
  export: Run
  init: Initialize
  modes: [session, beacon]
  repeat: 2
  command_timeout: 30s
  args_utf8: hello
  expect: {output: {contains: [hello]}}
suites:
  - name: linux-amd64
    target: {os: linux, arch: amd64}
    tests:
      - name: echo
        library: lib/echo.so
        args_base64: AAEC
`)
	loaded, err := Load(manifestPath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Plans) != 1 || loaded.Plans[0].Library != canonical(t, library) || loaded.Plans[0].LibraryPath != "lib/echo.so" {
		t.Fatalf("plans = %#v", loaded.Plans)
	}
	if loaded.Plans[0].Steps != nil {
		t.Fatalf("legacy test gained sequence steps: %#v", loaded.Plans[0].Steps)
	}
	settings := loaded.Plans[0].Settings
	if settings.Export != "Run" || settings.Init != "Initialize" || string(settings.Arguments) != string([]byte{0, 1, 2}) || settings.Repeat != 2 || settings.CommandTimeout.String() != "30s" {
		t.Fatalf("normalized settings = %#v", settings)
	}
	if len(settings.Modes) != 2 || settings.Modes[0] != ModeSession || settings.Modes[1] != ModeBeacon || !equalStrings(settings.Expect.Output.Contains, []string{"hello"}) {
		t.Fatalf("normalized modes/expect = %#v", settings)
	}
	if matrix := loaded.Matrix(); len(matrix.Include) != 1 || matrix.Include[0].Runner != "ubuntu-24.04" {
		t.Fatalf("matrix = %#v", matrix)
	}
}

func TestLegacyArgumentsKeepLiteralPlaceholders(t *testing.T) {
	workspace := t.TempDir()
	writeELFSharedObject(t, workspace, "lib/echo.so", 2, 62)
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: master}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    tests:
      - name: literal-argument
        library: lib/echo.so
        export: Run
        args_utf8: '${TCP_PORT}'
        expect: {success_only: true}
`)
	loaded, err := Load(manifestPath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(loaded.Plans[0].Settings.Arguments); got != "${TCP_PORT}" {
		t.Fatalf("literal legacy arguments = %q", got)
	}
}

func TestLoadOrderedStepsAndProbes(t *testing.T) {
	workspace := t.TempDir()
	writeELFSharedObject(t, workspace, "lib/echo.so", 2, 62)
	probePath := writeFile(t, workspace, "test/probe.sh", []byte("#!/bin/sh\nexit 0\n"))
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: master}
defaults:
  export: Run
  command_timeout: 25s
  args_utf8: ignored-default-args
  expect: {success_only: true}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    tests:
      - name: lifecycle
        library: lib/echo.so
        modes: [session, beacon]
        steps:
          - name: start
            args_utf8: 'start --port ${TCP_PORT}'
            expect: {output: {contains: [started]}}
            tcp_probe: {port: auto, state: open, banner_utf8: "RFB 003.008\n"}
            command_probe:
              path: test/probe.sh
              args: ['${TCP_PORT}', verify]
          - name: stop
            export: Stop
            args_base64: AAEC
            command_timeout: 5s
            expect: {output: {equals: stopped}}
            tcp_probe: {port: auto, state: closed, timeout: 15s}
`)
	loaded, err := LoadForPlanning(manifestPath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	plan := loaded.Plans[0]
	if len(plan.Steps) != 2 || plan.Settings.Arguments != nil || validExpectation(plan.Settings.Expect) {
		t.Fatalf("sequenced plan = %#v", plan)
	}
	start, stop := plan.Steps[0], plan.Steps[1]
	if start.Name != "start" || start.Export != "Run" || start.CommandTimeout.String() != "25s" || !start.InterpolatePort || string(start.Arguments) != "start --port ${TCP_PORT}" {
		t.Fatalf("start step = %#v", start)
	}
	if start.TCPProbe == nil || !start.TCPProbe.AutoPort || start.TCPProbe.State != "open" || start.TCPProbe.Timeout.String() != "10s" || string(start.TCPProbe.Banner) != "RFB 003.008\n" {
		t.Fatalf("start tcp probe = %#v", start.TCPProbe)
	}
	if start.CommandProbe == nil || start.CommandProbe.Path != canonical(t, probePath) || start.CommandProbe.Timeout.String() != "30s" || !equalStrings(start.CommandProbe.Args, []string{"${TCP_PORT}", "verify"}) {
		t.Fatalf("start command probe = %#v", start.CommandProbe)
	}
	if stop.Name != "stop" || stop.Export != "Stop" || stop.CommandTimeout.String() != "5s" || string(stop.Arguments) != string([]byte{0, 1, 2}) || stop.TCPProbe == nil || stop.TCPProbe.State != "closed" || stop.TCPProbe.Timeout.String() != "15s" {
		t.Fatalf("stop step = %#v", stop)
	}
	if len(loaded.Config.Suites[0].Tests[0].Steps) != 2 {
		t.Fatalf("normalized test steps = %#v", loaded.Config.Suites[0].Tests[0].Steps)
	}
	loaded.Config.Suites[0].Tests[0].Steps[0].Arguments[0] = 'X'
	if plan.Steps[0].Arguments[0] != 's' {
		t.Fatal("normalized test and plan steps alias argument storage")
	}
}

func TestLoadRejectsInvalidOrderedSteps(t *testing.T) {
	workspace := t.TempDir()
	writeELFSharedObject(t, workspace, "lib/echo.so", 2, 62)
	writeFile(t, workspace, "test/probe.sh", []byte("#!/bin/sh\nexit 0\n"))
	base := `
schema: sliver-ext-e2e/v1
sliver: {version: master}
defaults: {export: Run}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    tests:
      - name: lifecycle
        library: lib/echo.so
        steps:
          - name: start
            args_utf8: ping
            expect: {success_only: true}
`
	for _, test := range []struct{ name, manifest, want string }{
		{"empty sequence", strings.Replace(base, "        steps:\n          - name: start\n            args_utf8: ping\n            expect: {success_only: true}", "        steps: []", 1), "must contain between 1 and 32 steps"},
		{"duplicate step", base + "          - name: start\n            expect: {success_only: true}\n", ".name \"start\" is duplicated"},
		{"missing step expect", strings.Replace(base, "            expect: {success_only: true}\n", "", 1), ".expect is required"},
		{"missing effective export", strings.Replace(base, "defaults: {export: Run}\n", "", 1), ".export is required"},
		{"test-level arguments", strings.Replace(base, "        steps:\n", "        args_utf8: legacy\n        steps:\n", 1), "cannot set test-level args_utf8"},
		{"test-level expectation", strings.Replace(base, "        steps:\n", "        expect: {success_only: true}\n        steps:\n", 1), "cannot set test-level args_utf8"},
		{"both step arguments", strings.Replace(base, "            args_utf8: ping\n", "            args_utf8: ping\n            args_base64: cGluZw==\n", 1), "cannot set both args_utf8 and args_base64"},
		{"unknown placeholder", strings.Replace(base, "args_utf8: ping", "args_utf8: '${OTHER}'", 1), "unsupported ${...} placeholder"},
		{"auto placeholder without probe", strings.Replace(base, "args_utf8: ping", "args_utf8: '${TCP_PORT}'", 1), "without a tcp_probe.port: auto"},
		{"invalid TCP port", strings.Replace(base, "            expect: {success_only: true}\n", "            expect: {success_only: true}\n            tcp_probe: {port: 0, state: open}\n", 1), ".port must be an integer"},
		{"closed probe banner", strings.Replace(base, "            expect: {success_only: true}\n", "            expect: {success_only: true}\n            tcp_probe: {port: 5901, state: closed, banner_utf8: RFB}\n", 1), "banner_utf8 requires state: open"},
		{"TCP probe timeout", strings.Replace(base, "            expect: {success_only: true}\n", "            expect: {success_only: true}\n            tcp_probe: {port: 5901, state: open, timeout: 3m}\n", 1), ".timeout must be greater than zero"},
		{"command probe escape", strings.Replace(base, "            expect: {success_only: true}\n", "            expect: {success_only: true}\n            command_probe: {path: ../escape.sh}\n", 1), "must be a clean relative path"},
		{"command probe partial placeholder", strings.Replace(base, "            expect: {success_only: true}\n", "            expect: {success_only: true}\n            tcp_probe: {port: auto, state: open}\n            command_probe: {path: test/probe.sh, args: ['--port=${TCP_PORT}']}\n", 1), "unsupported or partial ${...} placeholder"},
		{"command probe auto without TCP probe", strings.Replace(base, "            expect: {success_only: true}\n", "            expect: {success_only: true}\n            command_probe: {path: test/probe.sh, args: ['${TCP_PORT}']}\n", 1), "without a tcp_probe.port: auto"},
		{"command probe timeout", strings.Replace(base, "            expect: {success_only: true}\n", "            expect: {success_only: true}\n            command_probe: {path: test/probe.sh, timeout: 3m}\n", 1), ".timeout must be greater than zero"},
		{"step unsupported field", strings.Replace(base, "            expect: {success_only: true}\n", "            expect: {success_only: true}\n            repeat: 2\n", 1), "field repeat not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, workspace, test.manifest), workspace)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, test.want)
			}
		})
	}
	tooMany := strings.Replace(base,
		"          - name: start\n            args_utf8: ping\n            expect: {success_only: true}\n",
		strings.Repeat("          - name: start\n            expect: {success_only: true}\n", 33), 1)
	if _, err := Load(writeManifest(t, workspace, tooMany), workspace); err == nil || !strings.Contains(err.Error(), "must contain between 1 and 32 steps") {
		t.Fatalf("33-step sequence error = %v", err)
	}
}

func TestPlannerRejectsCommandProbeSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated Windows privileges")
	}
	workspace := t.TempDir()
	writeELFSharedObject(t, workspace, "lib/echo.so", 2, 62)
	real := writeFile(t, workspace, "test/real.sh", []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Symlink(real, filepath.Join(workspace, "test", "link.sh")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: master}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    tests:
      - name: lifecycle
        library: lib/echo.so
        steps:
          - name: call
            export: Run
            expect: {success_only: true}
            command_probe: {path: test/link.sh}
`)
	if _, err := Load(manifestPath, workspace); err != nil {
		t.Fatalf("direct load rejected confined probe symlink: %v", err)
	}
	if _, err := LoadForPlanning(manifestPath, workspace); err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("planner probe symlink error = %v", err)
	}
}

func TestCollectionOverridesInheritAndReplaceArguments(t *testing.T) {
	workspace := t.TempDir()
	writeELFSharedObject(t, workspace, "lib/alpha.so", 2, 62)
	writeELFSharedObject(t, workspace, "lib/beta.so", 2, 62)
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: latest}
defaults:
  args_utf8: default
  expect: {success_only: true}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    collections:
      - name: shared
        glob: lib/*.so
        export: Go
        overrides:
          beta.so:
            export: Other
            args_base64: /w==
            expect: {output: {sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}}
`)
	loaded, err := Load(manifestPath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Plans) != 2 || loaded.Plans[0].Name != "alpha.so" || loaded.Plans[1].Name != "beta.so" {
		t.Fatalf("plans = %#v", loaded.Plans)
	}
	if loaded.Plans[0].Settings.Export != "Go" || string(loaded.Plans[0].Settings.Arguments) != "default" || !loaded.Plans[0].Settings.Expect.SuccessOnly {
		t.Fatalf("alpha settings = %#v", loaded.Plans[0].Settings)
	}
	if loaded.Plans[1].Settings.Export != "Other" || string(loaded.Plans[1].Settings.Arguments) != string([]byte{0xff}) || loaded.Plans[1].Settings.Expect.SuccessOnly {
		t.Fatalf("beta settings = %#v", loaded.Plans[1].Settings)
	}
	if loaded.Plans[1].ID != "linux/shared/beta.so" {
		t.Fatalf("beta plan ID = %q", loaded.Plans[1].ID)
	}
}

func TestLoadRejectsInvalidExtensionManifest(t *testing.T) {
	workspace := t.TempDir()
	library := writeELFSharedObject(t, workspace, "lib/echo.so", 2, 62)
	writeFile(t, workspace, "lib/echo.dll", readFile(t, library))
	base := `
schema: sliver-ext-e2e/v1
sliver: {version: master}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    tests:
      - name: echo
        library: lib/echo.so
        export: Run
        expect: {success_only: true}
`
	for _, test := range []struct{ name, manifest, want string }{
		{"unknown field", strings.Replace(base, "        expect:", "        mystery: true\n        expect:", 1), "field mystery not found"},
		{"wrong schema", strings.Replace(base, SchemaV1, "sliver-ext-e2e/v2", 1), `schema must be "sliver-ext-e2e/v1"`},
		{"missing export", strings.Replace(base, "        export: Run\n", "", 1), ".export is required"},
		{"missing expect", strings.Replace(base, "        expect: {success_only: true}\n", "", 1), ".expect must explicitly"},
		{"both arg encodings", strings.Replace(base, "        expect:", "        args_utf8: abc\n        args_base64: YWJj\n        expect:", 1), "cannot set both args_utf8 and args_base64"},
		{"bad base64", strings.Replace(base, "        expect:", "        args_base64: '*bad*'\n        expect:", 1), "must be canonical base64"},
		{"duplicate mode", strings.Replace(base, "        expect:", "        modes: [session, session]\n        expect:", 1), "duplicate mode"},
		{"wrong suffix", strings.Replace(base, "lib/echo.so", "lib/echo.dll", 1), "must have a .so"},
		{"success and output", strings.Replace(base, "expect: {success_only: true}", "expect: {success_only: true, output: {equals: ok}}", 1), "success_only cannot be true"},
		{"bad regexp", strings.Replace(base, "expect: {success_only: true}", "expect: {output: {matches: ['[']}}", 1), "not a valid Go regexp"},
		{"unknown fixture", strings.Replace(base, "suites:", "fixtures: {}\nsuites:", 1), "field fixtures not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, workspace, test.manifest), workspace)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLibraryPathIsolation(t *testing.T) {
	workspace := t.TempDir()
	libraryRoot := t.TempDir()
	writeELFSharedObject(t, workspace, "lib/stale.so", 2, 62)
	writeELFSharedObject(t, libraryRoot, "lib/planned.so", 2, 62)
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: master}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    collections:
      - name: exact
        glob: lib/*.so
        export: Run
        expect: {success_only: true}
`)
	loaded, err := LoadWithLibraryRoot(manifestPath, workspace, libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Plans) != 1 || loaded.Plans[0].LibraryPath != "lib/planned.so" {
		t.Fatalf("isolated plans = %#v", loaded.Plans)
	}
	if _, err := LoadWithLibraryRoot(manifestPath, workspace, workspace); err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("overlap error = %v", err)
	}
	writeFile(t, libraryRoot, "unexpected.txt", []byte("unexpected"))
	if _, err := LoadWithLibraryRoot(manifestPath, workspace, libraryRoot); err == nil || !strings.Contains(err.Error(), "non-library file") {
		t.Fatalf("unplanned file error = %v", err)
	}
}

func TestPlannerRejectsLibrarySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated Windows privileges")
	}
	workspace := t.TempDir()
	real := writeELFSharedObject(t, workspace, "lib/real.so", 2, 62)
	if err := os.Symlink(real, filepath.Join(workspace, "lib", "link.so")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: master}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    tests:
      - name: link
        library: lib/link.so
        export: Run
        expect: {success_only: true}
`)
	if _, err := Load(manifestPath, workspace); err != nil {
		t.Fatalf("direct load rejected confined symlink: %v", err)
	}
	if _, err := LoadForPlanning(manifestPath, workspace); err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("planner symlink error = %v", err)
	}
}

func TestLoadRejectsLibrarySymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated Windows privileges")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	real := writeELFSharedObject(t, outside, "outside.so", 2, 62)
	if err := os.Symlink(real, filepath.Join(workspace, "escape.so")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: master}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    tests:
      - name: escape
        library: escape.so
        export: Run
        expect: {success_only: true}
`)
	if _, err := Load(manifestPath, workspace); err == nil || !strings.Contains(err.Error(), "resolves outside workspace") {
		t.Fatalf("escape error = %v", err)
	}
}

func TestCollectionRejectsAmbiguousOverrides(t *testing.T) {
	workspace := t.TempDir()
	writeELFSharedObject(t, workspace, "lib/one/same.so", 2, 62)
	writeELFSharedObject(t, workspace, "lib/two/same.so", 2, 62)
	manifestPath := writeManifest(t, workspace, `
schema: sliver-ext-e2e/v1
sliver: {version: master}
suites:
  - name: linux
    target: {os: linux, arch: amd64}
    collections:
      - name: all
        glob: lib/*/*.so
        export: Run
        expect: {success_only: true}
        overrides:
          missing.so: {repeat: 2}
`)
	if _, err := Load(manifestPath, workspace); err == nil || !strings.Contains(err.Error(), "duplicate basename") {
		t.Fatalf("duplicate basename error = %v", err)
	}
}

func TestSchemaFileIsValidJSON(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "schema", "sliver-ext-e2e-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(contents, &value); err != nil {
		t.Fatalf("invalid schema JSON: %v", err)
	}
	var schema struct {
		Definitions map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(contents, &schema); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test", "step", "tcpProbe", "commandProbe"} {
		if len(schema.Definitions[name]) == 0 {
			t.Errorf("schema is missing %q definition", name)
		}
	}
}

func writeManifest(t *testing.T, workspace, contents string) string {
	t.Helper()
	path := filepath.Join(workspace, strings.ReplaceAll(t.Name(), "/", "-")+"-manifest.yml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, workspace, relative string, contents []byte) string {
	t.Helper()
	path := filepath.Join(workspace, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(abs)
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func ExampleManifest_Matrix() {
	loaded := &Manifest{Plans: []Plan{{Target: Target{OS: "windows", Arch: "amd64"}}}}
	encoded, _ := json.Marshal(loaded.Matrix())
	fmt.Println(string(encoded))
	// Output: {"include":[{"target":"windows/amd64","runner":"windows-2022","key":"windows-amd64"}]}
}
