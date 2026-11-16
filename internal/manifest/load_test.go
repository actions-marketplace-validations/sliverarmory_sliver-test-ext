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
