package manifest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sliverarmory/sliver-test-ext/internal/safepath"
	"gopkg.in/yaml.v3"
)

// Load decodes, validates, normalizes, and expands a manifest for direct action
// use. Manifest and library paths resolve from workspace.
func Load(path, workspace string) (*Manifest, error) {
	return load(path, workspace, workspace, false, false)
}

// LoadForPlanning rejects library symlinks so planner-staged artifacts have a
// stable workspace-relative destination on every supported filesystem.
func LoadForPlanning(path, workspace string) (*Manifest, error) {
	return load(path, workspace, workspace, true, false)
}

// LoadWithLibraryRoot resolves libraries and collection globs from a private,
// symlink-free artifact tree while keeping the manifest in the caller workspace.
func LoadWithLibraryRoot(path, workspace, libraryRoot string) (*Manifest, error) {
	return load(path, workspace, libraryRoot, true, true)
}

func load(path, workspace, libraryRoot string, rejectLibrarySymlinks, isolatedLibraryRoot bool) (*Manifest, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
	}
	libraries, err := newLibraryPathResolver(libraryRoot, rejectLibrarySymlinks)
	if err != nil {
		return nil, fmt.Errorf("library root: %w", err)
	}
	if isolatedLibraryRoot && (pathWithin(paths.root, libraries.root) || pathWithin(libraries.root, paths.root)) {
		return nil, fmt.Errorf("isolated library root must not overlap the caller workspace")
	}

	manifestPath, err := paths.file(path, "manifest")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()

	var raw rawConfig
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("manifest is empty")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("manifest must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode trailing YAML document: %w", err)
	}

	config, plans, err := normalize(raw, libraries)
	if err != nil {
		return nil, err
	}
	if isolatedLibraryRoot {
		if err := validateIsolatedLibraryRoot(libraries.root, plans); err != nil {
			return nil, err
		}
	}
	return &Manifest{
		Workspace: paths.root,
		Path:      manifestPath,
		Config:    config,
		Plans:     plans,
	}, nil
}

func validateIsolatedLibraryRoot(root string, plans []Plan) error {
	expected := make(map[string]string, len(plans))
	for _, plan := range plans {
		folded := strings.ToUpper(plan.LibraryPath)
		if previous, ok := expected[folded]; ok && previous != plan.LibraryPath {
			return fmt.Errorf("isolated library paths %q and %q collide on case-insensitive filesystems", previous, plan.LibraryPath)
		}
		expected[folded] = plan.LibraryPath
	}
	seen := make(map[string]struct{}, len(expected))
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("isolated library root contains symlink %q", relative)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !isLibrarySuffix(relative) {
			return fmt.Errorf("isolated library root contains non-library file %q", relative)
		}
		if err := safepath.ValidatePortableRelative(relative); err != nil {
			return fmt.Errorf("isolated library path %q: %w", relative, err)
		}
		folded := strings.ToUpper(relative)
		planned, ok := expected[folded]
		if !ok || planned != relative {
			return fmt.Errorf("isolated library root contains unplanned library %q", relative)
		}
		seen[folded] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate isolated library root: %w", err)
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("isolated library root contains %d planned library file(s); expected %d", len(seen), len(expected))
	}
	return nil
}
