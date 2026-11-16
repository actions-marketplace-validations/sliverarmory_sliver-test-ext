package manifest

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxRepeat      = 1000
	maxTimeout     = 24 * time.Hour
	maxInlineBytes = 16 << 20
)

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func normalize(raw rawConfig, libraries pathResolver) (Config, []Plan, error) {
	if raw.Schema != SchemaV1 {
		return Config{}, nil, fmt.Errorf("schema must be %q", SchemaV1)
	}
	if err := validateVersion(raw.Sliver.Version); err != nil {
		return Config{}, nil, fmt.Errorf("sliver.version: %w", err)
	}
	if len(raw.Suites) == 0 {
		return Config{}, nil, fmt.Errorf("suites must contain at least one suite")
	}

	defaults, err := normalizeDefaults(raw.Defaults)
	if err != nil {
		return Config{}, nil, err
	}
	version := raw.Sliver.Version
	if commitPattern.MatchString(version) {
		version = strings.ToLower(version)
	}
	config := Config{
		Schema: raw.Schema, Sliver: SliverConfig{Version: version},
		Defaults: defaults, Suites: make([]Suite, 0, len(raw.Suites)),
	}
	plans := make([]Plan, 0)
	suiteNames := make(map[string]struct{}, len(raw.Suites))
	planIDs := make(map[string]struct{})

	for suiteIndex, input := range raw.Suites {
		where := fmt.Sprintf("suites[%d]", suiteIndex)
		if err := validateName(input.Name); err != nil {
			return Config{}, nil, fmt.Errorf("%s.name: %w", where, err)
		}
		if _, exists := suiteNames[input.Name]; exists {
			return Config{}, nil, fmt.Errorf("%s.name %q is duplicated", where, input.Name)
		}
		suiteNames[input.Name] = struct{}{}

		target := Target{OS: input.Target.OS, Arch: input.Target.Arch}
		if _, err := RunnerForTarget(target); err != nil {
			return Config{}, nil, fmt.Errorf("%s.target: %w", where, err)
		}
		if len(input.Tests) == 0 && len(input.Collections) == 0 {
			return Config{}, nil, fmt.Errorf("%s must contain at least one test or collection", where)
		}
		suite := Suite{
			Name: input.Name, Target: target,
			Tests: make([]Test, 0, len(input.Tests)), Collections: make([]Collection, 0, len(input.Collections)),
		}
		itemNames := make(map[string]struct{}, len(input.Tests)+len(input.Collections))

		for testIndex, inputTest := range input.Tests {
			testWhere := fmt.Sprintf("%s.tests[%d]", where, testIndex)
			if err := validateName(inputTest.Name); err != nil {
				return Config{}, nil, fmt.Errorf("%s.name: %w", testWhere, err)
			}
			if _, exists := itemNames[inputTest.Name]; exists {
				return Config{}, nil, fmt.Errorf("%s.name %q is duplicated in suite %q", testWhere, inputTest.Name, input.Name)
			}
			itemNames[inputTest.Name] = struct{}{}
			library, libraryPath, err := libraries.declaredLibrary(inputTest.Library, testWhere+".library")
			if err != nil {
				return Config{}, nil, err
			}
			if err := validateLibrary(library, target); err != nil {
				return Config{}, nil, fmt.Errorf("%s.library: %w", testWhere, err)
			}
			settings, err := normalizeSettings(defaults, inputTest.rawSettings, testWhere, true)
			if err != nil {
				return Config{}, nil, err
			}
			suite.Tests = append(suite.Tests, Test{Name: inputTest.Name, Library: library, Settings: cloneSettings(settings)})
			plan := Plan{
				ID: input.Name + "/" + inputTest.Name, Suite: input.Name, Name: inputTest.Name,
				Library: library, LibraryPath: libraryPath, Target: target, Settings: settings,
			}
			if err := appendUniquePlan(&plans, planIDs, plan); err != nil {
				return Config{}, nil, err
			}
		}

		for collectionIndex, inputCollection := range input.Collections {
			collectionWhere := fmt.Sprintf("%s.collections[%d]", where, collectionIndex)
			if err := validateName(inputCollection.Name); err != nil {
				return Config{}, nil, fmt.Errorf("%s.name: %w", collectionWhere, err)
			}
			if _, exists := itemNames[inputCollection.Name]; exists {
				return Config{}, nil, fmt.Errorf("%s.name %q is duplicated in suite %q", collectionWhere, inputCollection.Name, input.Name)
			}
			itemNames[inputCollection.Name] = struct{}{}
			collectionSettings, err := normalizeSettings(defaults, inputCollection.rawSettings, collectionWhere, false)
			if err != nil {
				return Config{}, nil, err
			}
			matches, err := libraries.glob(inputCollection.Glob, collectionWhere+".glob")
			if err != nil {
				return Config{}, nil, err
			}
			sort.Slice(matches, func(i, j int) bool { return matches[i].basename < matches[j].basename })
			libraryPaths := make([]string, 0, len(matches))
			basenames := make(map[string]string, len(matches))
			for _, match := range matches {
				folded := strings.ToUpper(match.basename)
				if previous, exists := basenames[folded]; exists {
					return Config{}, nil, fmt.Errorf("%s glob has duplicate basename %q and %q; overrides would be ambiguous", collectionWhere, previous, match.basename)
				}
				basenames[folded] = match.basename
				if err := validateLibrary(match.path, target); err != nil {
					return Config{}, nil, fmt.Errorf("%s library %q: %w", collectionWhere, match.basename, err)
				}
				libraryPaths = append(libraryPaths, match.path)
				settings := cloneSettings(collectionSettings)
				if override, ok := inputCollection.Overrides[match.basename]; ok {
					settings, err = normalizeSettings(settings, override, collectionWhere+".overrides["+match.basename+"]", true)
				} else {
					err = validateCompleteSettings(settings, collectionWhere+" library "+match.basename)
				}
				if err != nil {
					return Config{}, nil, err
				}
				plan := Plan{
					ID:    input.Name + "/" + inputCollection.Name + "/" + match.basename,
					Suite: input.Name, Name: match.basename, Collection: inputCollection.Name,
					Library: match.path, LibraryPath: match.workspacePath, Target: target, Settings: settings,
				}
				if err := appendUniquePlan(&plans, planIDs, plan); err != nil {
					return Config{}, nil, err
				}
			}
			for basename := range inputCollection.Overrides {
				if filepath.Base(basename) != basename || strings.ContainsAny(basename, "/\\") || basename == "." || basename == ".." {
					return Config{}, nil, fmt.Errorf("%s.overrides key %q must be a basename", collectionWhere, basename)
				}
				if actual, exists := basenames[strings.ToUpper(basename)]; !exists || actual != basename {
					return Config{}, nil, fmt.Errorf("%s.overrides key %q does not match a collection library", collectionWhere, basename)
				}
			}
			suite.Collections = append(suite.Collections, Collection{
				Name: inputCollection.Name, Glob: filepath.ToSlash(inputCollection.Glob),
				Settings: cloneSettings(collectionSettings), Libraries: libraryPaths,
			})
		}
		config.Suites = append(config.Suites, suite)
	}
	return config, plans, nil
}

func normalizeDefaults(raw rawSettings) (Settings, error) {
	base := Settings{Modes: []string{ModeSession, ModeBeacon}, Repeat: 1, CommandTimeout: 2 * time.Minute}
	return normalizeSettings(base, raw, "defaults", false)
}

func normalizeSettings(base Settings, raw rawSettings, where string, complete bool) (Settings, error) {
	settings := cloneSettings(base)
	if raw.Export != nil {
		settings.Export = *raw.Export
	}
	if raw.Init != nil {
		settings.Init = *raw.Init
	}
	if raw.Modes != nil {
		settings.Modes = append([]string(nil), (*raw.Modes)...)
	}
	if raw.Repeat != nil {
		settings.Repeat = *raw.Repeat
	}
	if raw.CommandTimeout != nil {
		duration, err := time.ParseDuration(*raw.CommandTimeout)
		if err != nil {
			return Settings{}, fmt.Errorf("%s.command_timeout: %w", where, err)
		}
		settings.CommandTimeout = duration
	}
	if raw.ArgsUTF8 != nil && raw.ArgsBase64 != nil {
		return Settings{}, fmt.Errorf("%s cannot set both args_utf8 and args_base64", where)
	}
	if raw.ArgsUTF8 != nil {
		if !utf8.ValidString(*raw.ArgsUTF8) {
			return Settings{}, fmt.Errorf("%s.args_utf8 must contain valid UTF-8", where)
		}
		if len(*raw.ArgsUTF8) > maxInlineBytes {
			return Settings{}, fmt.Errorf("%s.args_utf8 exceeds %d bytes", where, maxInlineBytes)
		}
		settings.Arguments = []byte(*raw.ArgsUTF8)
	}
	if raw.ArgsBase64 != nil {
		decoded, err := decodeBase64(*raw.ArgsBase64, where+".args_base64")
		if err != nil {
			return Settings{}, err
		}
		settings.Arguments = decoded
	}
	if raw.Expect != nil {
		expect, err := normalizeExpectation(*raw.Expect, where+".expect")
		if err != nil {
			return Settings{}, err
		}
		settings.Expect = expect
	}
	if err := validateCoreSettings(settings, where); err != nil {
		return Settings{}, err
	}
	if complete {
		if err := validateCompleteSettings(settings, where); err != nil {
			return Settings{}, err
		}
	}
	return settings, nil
}

func validateCoreSettings(settings Settings, where string) error {
	if strings.ContainsRune(settings.Export, '\x00') || (settings.Export != "" && strings.TrimSpace(settings.Export) != settings.Export) {
		return fmt.Errorf("%s.export must not contain NUL or surrounding whitespace", where)
	}
	if strings.ContainsRune(settings.Init, '\x00') {
		return fmt.Errorf("%s.init must not contain NUL bytes", where)
	}
	if len(settings.Modes) == 0 {
		return fmt.Errorf("%s.modes must contain at least one mode", where)
	}
	seen := make(map[string]struct{}, len(settings.Modes))
	for _, mode := range settings.Modes {
		if mode != ModeSession && mode != ModeBeacon {
			return fmt.Errorf("%s.modes contains %q; only %q and %q are supported", where, mode, ModeSession, ModeBeacon)
		}
		if _, exists := seen[mode]; exists {
			return fmt.Errorf("%s.modes contains duplicate mode %q", where, mode)
		}
		seen[mode] = struct{}{}
	}
	if settings.Repeat < 1 || settings.Repeat > maxRepeat {
		return fmt.Errorf("%s.repeat must be between 1 and %d", where, maxRepeat)
	}
	if settings.CommandTimeout <= 0 || settings.CommandTimeout > maxTimeout {
		return fmt.Errorf("%s.command_timeout must be greater than zero and no more than %s", where, maxTimeout)
	}
	return nil
}

func validateCompleteSettings(settings Settings, where string) error {
	if strings.TrimSpace(settings.Export) == "" {
		return fmt.Errorf("%s.export is required", where)
	}
	if !validExpectation(settings.Expect) {
		return fmt.Errorf("%s.expect must explicitly set success_only: true or at least one output assertion", where)
	}
	return nil
}

func normalizeExpectation(raw rawExpectation, where string) (Expectation, error) {
	expect := Expectation{}
	if raw.SuccessOnly != nil {
		expect.SuccessOnly = *raw.SuccessOnly
	}
	if raw.Output != nil {
		output, err := normalizeOutputExpectation(*raw.Output, where+".output")
		if err != nil {
			return Expectation{}, err
		}
		expect.Output = output
	}
	if expect.SuccessOnly && raw.Output != nil {
		return Expectation{}, fmt.Errorf("%s.success_only cannot be true when output assertions are set", where)
	}
	if !validExpectation(expect) {
		return Expectation{}, fmt.Errorf("%s must explicitly set success_only: true or at least one output assertion", where)
	}
	return expect, nil
}

func normalizeOutputExpectation(raw rawOutputExpectation, where string) (OutputExpectation, error) {
	output := OutputExpectation{
		Equals: raw.Equals, Contains: append([]string(nil), raw.Contains...),
		NotContains: append([]string(nil), raw.NotContains...), Matches: append([]string(nil), raw.Matches...),
	}
	for index, value := range output.Contains {
		if value == "" {
			return OutputExpectation{}, fmt.Errorf("%s.contains[%d] cannot be empty", where, index)
		}
	}
	for index, value := range output.NotContains {
		if value == "" {
			return OutputExpectation{}, fmt.Errorf("%s.not_contains[%d] cannot be empty", where, index)
		}
	}
	for index, pattern := range output.Matches {
		if pattern == "" {
			return OutputExpectation{}, fmt.Errorf("%s.matches[%d] cannot be empty", where, index)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return OutputExpectation{}, fmt.Errorf("%s.matches[%d] is not a valid Go regexp: %w", where, index, err)
		}
	}
	if raw.SHA256 != "" {
		if len(raw.SHA256) != 64 {
			return OutputExpectation{}, fmt.Errorf("%s.sha256 must contain exactly 64 hexadecimal characters", where)
		}
		if _, err := hex.DecodeString(raw.SHA256); err != nil {
			return OutputExpectation{}, fmt.Errorf("%s.sha256 must be hexadecimal", where)
		}
		output.SHA256 = strings.ToLower(raw.SHA256)
	}
	if raw.EqualsBase64 != nil {
		if raw.Equals != nil {
			return OutputExpectation{}, fmt.Errorf("%s cannot set both equals and equals_base64", where)
		}
		decoded, err := decodeBase64(*raw.EqualsBase64, where+".equals_base64")
		if err != nil {
			return OutputExpectation{}, err
		}
		output.EqualsBytes = decoded
		output.HasEqualsB64 = true
	}
	return output, nil
}

func validExpectation(expect Expectation) bool {
	return expect.SuccessOnly || hasOutputExpectation(expect.Output)
}

func hasOutputExpectation(output OutputExpectation) bool {
	return output.Equals != nil || len(output.Contains) != 0 || len(output.NotContains) != 0 ||
		len(output.Matches) != 0 || output.SHA256 != "" || output.HasEqualsB64
}

func cloneSettings(input Settings) Settings {
	output := input
	if input.Expect.Output.Equals != nil {
		equals := *input.Expect.Output.Equals
		output.Expect.Output.Equals = &equals
	}
	output.Modes = append([]string(nil), input.Modes...)
	output.Arguments = append([]byte(nil), input.Arguments...)
	output.Expect.Output.Contains = append([]string(nil), input.Expect.Output.Contains...)
	output.Expect.Output.NotContains = append([]string(nil), input.Expect.Output.NotContains...)
	output.Expect.Output.Matches = append([]string(nil), input.Expect.Output.Matches...)
	output.Expect.Output.EqualsBytes = append([]byte(nil), input.Expect.Output.EqualsBytes...)
	return output
}

func decodeBase64(value, where string) ([]byte, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("%s must be canonical base64", where)
	}
	if len(decoded) > maxInlineBytes {
		return nil, fmt.Errorf("%s decodes to more than %d bytes", where, maxInlineBytes)
	}
	return decoded, nil
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("must be non-empty")
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("must not have leading or trailing whitespace")
	}
	if strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("must not contain path separators or NUL bytes")
	}
	return nil
}

func validateVersion(version string) error {
	if version == "latest" || version == "master" || versionPattern.MatchString(version) || commitPattern.MatchString(version) {
		return nil
	}
	return fmt.Errorf("must be latest, master, a vX.Y.Z release tag, or a full 40-character commit SHA")
}

func appendUniquePlan(plans *[]Plan, ids map[string]struct{}, plan Plan) error {
	if _, exists := ids[plan.ID]; exists {
		return fmt.Errorf("expanded plan ID %q is duplicated", plan.ID)
	}
	ids[plan.ID] = struct{}{}
	*plans = append(*plans, plan)
	return nil
}
