package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sliverarmory/sliver-test-ext/internal/manifest"
	"github.com/sliverarmory/sliver-test-ext/internal/safepath"
)

type stagedLibrary struct {
	plannedLibrary
	data []byte
}

// stagePlanLibraries copies only manifest-expanded extension libraries into an empty,
// action-owned staging directory while preserving their workspace-relative
// paths. The resulting directory is safe to upload as the workflow's library
// artifact; no manifest, source, credential, or other workspace file enters it.
func stagePlanLibraries(workspace, destination string, plans []manifest.Plan) ([]plannedLibrary, error) {
	workspace, err := canonicalDirectory(workspace)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	libraries := make([]stagedLibrary, 0, len(plans))
	seenPaths := make(map[string]string, len(plans))
	for _, plan := range plans {
		relative := filepath.ToSlash(plan.LibraryPath)
		if err := validateStagedLibraryPath(relative); err != nil {
			return nil, fmt.Errorf("%s library path %q: %w", plan.ID, relative, err)
		}
		folded := strings.ToUpper(relative)
		if previous, ok := seenPaths[folded]; ok {
			if previous != relative {
				return nil, fmt.Errorf("library paths %q and %q collide on case-insensitive filesystems", previous, relative)
			}
			continue
		}
		seenPaths[folded] = relative

		source, err := filepath.Abs(plan.Library)
		if err != nil {
			return nil, fmt.Errorf("resolve %s library source: %w", plan.ID, err)
		}
		info, err := os.Lstat(source)
		if err != nil {
			return nil, fmt.Errorf("inspect %s library: %w", plan.ID, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s library source must be a regular non-symlink file", plan.ID)
		}
		source, err = filepath.EvalSymlinks(source)
		if err != nil || !pathWithinDirectory(workspace, source) {
			return nil, fmt.Errorf("%s library source is outside the workspace", plan.ID)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read %s library: %w", plan.ID, err)
		}
		digest := sha256.Sum256(data)
		libraries = append(libraries, stagedLibrary{
			plannedLibrary: plannedLibrary{Path: relative, SHA256: hex.EncodeToString(digest[:])},
			data:           data,
		})
	}
	if len(libraries) == 0 {
		return nil, fmt.Errorf("manifest contains no extension libraries")
	}
	sort.Slice(libraries, func(i, j int) bool { return libraries[i].Path < libraries[j].Path })

	if strings.TrimSpace(destination) != "" {
		root, err := validateStagingDirectory(workspace, destination)
		if err != nil {
			return nil, err
		}
		for _, library := range libraries {
			target := filepath.Join(root, filepath.FromSlash(library.Path))
			if !pathWithinDirectory(root, target) {
				return nil, fmt.Errorf("staged library path %q escapes destination", library.Path)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return nil, fmt.Errorf("create staged library parent: %w", err)
			}
			if err := os.WriteFile(target, library.data, 0o600); err != nil {
				return nil, fmt.Errorf("write staged library %q: %w", library.Path, err)
			}
			if err := os.Chmod(target, 0o600); err != nil {
				return nil, fmt.Errorf("set staged library permissions %q: %w", library.Path, err)
			}
		}
	}

	inventory := make([]plannedLibrary, len(libraries))
	for index := range libraries {
		inventory[index] = libraries[index].plannedLibrary
	}
	return inventory, nil
}

func validateStagedLibraryPath(value string) error {
	if err := safepath.ValidatePortableRelative(value); err != nil {
		return err
	}
	switch strings.ToLower(path.Ext(value)) {
	case ".dll", ".so", ".dylib":
	default:
		return fmt.Errorf("must have a .dll, .so, or .dylib extension")
	}
	return nil
}

func validateStagingDirectory(workspace, destination string) (string, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve staging directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := canonicalDirectory(filepath.Dir(absolute))
		if parentErr != nil {
			return "", fmt.Errorf("staging directory parent: %w", parentErr)
		}
		root := filepath.Join(parent, filepath.Base(absolute))
		if err := validateStagingLocation(workspace, root); err != nil {
			return "", err
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			return "", fmt.Errorf("create staging directory: %w", err)
		}
		absolute = root
		info, err = os.Lstat(absolute)
	}
	if err != nil {
		return "", fmt.Errorf("inspect staging directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("staging directory must not be a symlink")
	}
	root, err := canonicalDirectory(destination)
	if err != nil {
		return "", fmt.Errorf("staging directory: %w", err)
	}
	if err := validateStagingLocation(workspace, root); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read staging directory: %w", err)
	}
	if len(entries) != 0 {
		return "", fmt.Errorf("staging directory must be empty")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("set staging directory permissions: %w", err)
	}
	return root, nil
}

func validateStagingLocation(workspace, root string) error {
	if pathWithinDirectory(workspace, root) {
		return fmt.Errorf("staging directory must be outside the caller workspace")
	}
	if runnerTemp := strings.TrimSpace(os.Getenv("RUNNER_TEMP")); runnerTemp != "" {
		tempRoot, err := canonicalDirectory(runnerTemp)
		if err != nil {
			return fmt.Errorf("runner temp: %w", err)
		}
		if !pathWithinDirectory(tempRoot, root) {
			return fmt.Errorf("staging directory must be inside RUNNER_TEMP")
		}
	}
	return nil
}

func canonicalDirectory(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", value)
	}
	return filepath.Clean(real), nil
}

func pathWithinDirectory(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}
