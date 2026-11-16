package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sliverarmory/sliver-test-ext/internal/manifest"
)

func TestStagePlanLibrariesCopiesOnlyValidatedRelativeLibraries(t *testing.T) {
	workspace := t.TempDir()
	runnerTemp := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	first := writeStageLibrary(t, workspace, "build/windows/amd64/one.so", []byte("one"))
	second := writeStageLibrary(t, workspace, ".libraries/linux/386/two.so", []byte("two"))
	destination, err := os.MkdirTemp(runnerTemp, "libraries-")
	if err != nil {
		t.Fatal(err)
	}
	plans := []manifest.Plan{
		{ID: "second", Library: second, LibraryPath: ".libraries/linux/386/two.so"},
		{ID: "first", Library: first, LibraryPath: "build/windows/amd64/one.so"},
		{ID: "duplicate", Library: first, LibraryPath: "build/windows/amd64/one.so"},
	}
	inventory, err := stagePlanLibraries(workspace, destination, plans)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 2 || inventory[0].Path != ".libraries/linux/386/two.so" || inventory[1].Path != "build/windows/amd64/one.so" {
		t.Fatalf("inventory = %#v", inventory)
	}
	for _, item := range inventory {
		data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(item.Path)))
		if err != nil {
			t.Fatalf("read staged %s: %v", item.Path, err)
		}
		digest := sha256.Sum256(data)
		if got := hex.EncodeToString(digest[:]); got != item.SHA256 {
			t.Fatalf("staged %s digest = %s, want %s", item.Path, got, item.SHA256)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(item.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("staged %s mode = %o, want 600", item.Path, info.Mode().Perm())
			}
		}
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != ".libraries" || entries[1].Name() != "build" {
		t.Fatalf("staging root entries = %#v", entries)
	}
}

func TestStagePlanLibrariesCreatesMissingDestination(t *testing.T) {
	workspace := t.TempDir()
	runnerTemp := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	library := writeStageLibrary(t, workspace, "libraries/test.so", []byte("library"))
	destination := filepath.Join(runnerTemp, "planned-libraries")

	inventory, err := stagePlanLibraries(workspace, destination, []manifest.Plan{{
		ID: "test", Library: library, LibraryPath: "libraries/test.so",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 1 || inventory[0].Path != "libraries/test.so" {
		t.Fatalf("inventory = %#v", inventory)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "libraries", "test.so")); err != nil || string(data) != "library" {
		t.Fatalf("staged library = %q, %v", data, err)
	}
}

func TestStagePlanLibrariesRejectsUnsafeSourcesAndDestinations(t *testing.T) {
	workspace := t.TempDir()
	runnerTemp := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	library := writeStageLibrary(t, workspace, "library.so", []byte("library"))

	tests := []struct {
		name        string
		plan        manifest.Plan
		prepareDest func(*testing.T) string
		want        string
	}{
		{
			name: "path escape", plan: manifest.Plan{ID: "escape", Library: library, LibraryPath: "../library.so"},
			prepareDest: emptyStageDestination(runnerTemp), want: "clean relative path",
		},
		{
			name: "Windows reserved path", plan: manifest.Plan{ID: "reserved", Library: library, LibraryPath: "build/CON.so"},
			prepareDest: emptyStageDestination(runnerTemp), want: "Windows-reserved",
		},
		{
			name: "destination in workspace", plan: manifest.Plan{ID: "library", Library: library, LibraryPath: "library.so"},
			prepareDest: func(t *testing.T) string {
				path := filepath.Join(workspace, "stage")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			}, want: "outside the caller workspace",
		},
		{
			name: "nonempty destination", plan: manifest.Plan{ID: "library", Library: library, LibraryPath: "library.so"},
			prepareDest: func(t *testing.T) string {
				path := emptyStageDestination(runnerTemp)(t)
				if err := os.WriteFile(filepath.Join(path, "unexpected"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			}, want: "must be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := stagePlanLibraries(workspace, test.prepareDest(t), []manifest.Plan{test.plan})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("stagePlanLibraries() error = %v, want %q", err, test.want)
			}
		})
	}

	if runtime.GOOS != "windows" {
		symlink := filepath.Join(workspace, "link.so")
		if err := os.Symlink(library, symlink); err != nil {
			t.Fatal(err)
		}
		_, err := stagePlanLibraries(workspace, emptyStageDestination(runnerTemp)(t), []manifest.Plan{{ID: "link", Library: symlink, LibraryPath: "link.so"}})
		if err == nil || !strings.Contains(err.Error(), "non-symlink") {
			t.Fatalf("symlink error = %v", err)
		}

		destination := emptyStageDestination(runnerTemp)(t)
		symlinkDestination := destination + "-link"
		if err := os.Symlink(destination, symlinkDestination); err != nil {
			t.Fatal(err)
		}
		_, err = stagePlanLibraries(workspace, symlinkDestination, []manifest.Plan{{ID: "library", Library: library, LibraryPath: "library.so"}})
		if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
			t.Fatalf("staging destination symlink error = %v", err)
		}
	}
}

func emptyStageDestination(root string) func(*testing.T) string {
	return func(t *testing.T) string {
		t.Helper()
		path, err := os.MkdirTemp(root, "libraries-")
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
}

func writeStageLibrary(t *testing.T, workspace, relative string, data []byte) string {
	t.Helper()
	path := filepath.Join(workspace, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
