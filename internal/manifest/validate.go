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

	"github.com/sliverarmory/sliver-test-ext/internal/safepath"
	"gopkg.in/yaml.v3"
)

const (
	maxRepeat        = 1000
	maxTimeout       = 24 * time.Hour
	maxInlineBytes   = 16 << 20
	maxSequenceSteps = 32
	maxProbeTimeout  = 2 * time.Minute
	maxBannerBytes   = 4 << 10
	maxProbeArgs     = 32
	maxProbeArgBytes = 4 << 10
)

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func normalize(raw rawConfig, libraries, workspace pathResolver, rejectProbeSymlinks bool) (Config, []Plan, error) {
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
			sequenced := inputTest.Steps != nil
			if sequenced && (inputTest.ArgsUTF8 != nil || inputTest.ArgsBase64 != nil || inputTest.Expect != nil) {
				return Config{}, nil, fmt.Errorf("%s cannot set test-level args_utf8, args_base64, or expect when steps is present", testWhere)
			}
			settings, err := normalizeSettings(defaults, inputTest.rawSettings, testWhere, !sequenced)
			if err != nil {
				return Config{}, nil, err
			}
			var steps []Step
			if sequenced {
				settings.Arguments = nil
				settings.Expect = Expectation{}
				steps, err = normalizeSteps(*inputTest.Steps, settings, workspace, rejectProbeSymlinks, testWhere+".steps")
				if err != nil {
					return Config{}, nil, err
				}
			}
			suite.Tests = append(suite.Tests, Test{Name: inputTest.Name, Library: library, Settings: cloneSettings(settings), Steps: cloneSteps(steps)})
			plan := Plan{
				ID: input.Name + "/" + inputTest.Name, Suite: input.Name, Name: inputTest.Name,
				Library: library, LibraryPath: libraryPath, Target: target, Settings: settings, Steps: steps,
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

func normalizeSteps(raw []rawStep, base Settings, workspace pathResolver, rejectProbeSymlinks bool, where string) ([]Step, error) {
	if len(raw) == 0 || len(raw) > maxSequenceSteps {
		return nil, fmt.Errorf("%s must contain between 1 and %d steps", where, maxSequenceSteps)
	}
	steps := make([]Step, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	anyAutoPort := false
	for index, input := range raw {
		stepWhere := fmt.Sprintf("%s[%d]", where, index)
		if err := validateName(input.Name); err != nil {
			return nil, fmt.Errorf("%s.name: %w", stepWhere, err)
		}
		if _, exists := seen[input.Name]; exists {
			return nil, fmt.Errorf("%s.name %q is duplicated", stepWhere, input.Name)
		}
		seen[input.Name] = struct{}{}
		step := Step{Name: input.Name, Export: base.Export, CommandTimeout: base.CommandTimeout}
		if input.Export != nil {
			step.Export = *input.Export
		}
		if strings.TrimSpace(step.Export) == "" || strings.TrimSpace(step.Export) != step.Export || strings.ContainsRune(step.Export, '\x00') {
			return nil, fmt.Errorf("%s.export is required and must not contain NUL or surrounding whitespace", stepWhere)
		}
		if input.CommandTimeout != nil {
			duration, err := time.ParseDuration(*input.CommandTimeout)
			if err != nil {
				return nil, fmt.Errorf("%s.command_timeout: %w", stepWhere, err)
			}
			step.CommandTimeout = duration
		}
		if step.CommandTimeout <= 0 || step.CommandTimeout > maxTimeout {
			return nil, fmt.Errorf("%s.command_timeout must be greater than zero and no more than %s", stepWhere, maxTimeout)
		}
		arguments, interpolate, err := normalizeStepArguments(input.ArgsUTF8, input.ArgsBase64, stepWhere)
		if err != nil {
			return nil, err
		}
		step.Arguments, step.InterpolatePort = arguments, interpolate
		if input.Expect == nil {
			return nil, fmt.Errorf("%s.expect is required", stepWhere)
		}
		step.Expect, err = normalizeExpectation(*input.Expect, stepWhere+".expect")
		if err != nil {
			return nil, err
		}
		if input.TCPProbe != nil {
			step.TCPProbe, err = normalizeTCPProbe(*input.TCPProbe, stepWhere+".tcp_probe")
			if err != nil {
				return nil, err
			}
			anyAutoPort = anyAutoPort || step.TCPProbe.AutoPort
		}
		if input.CommandProbe != nil {
			step.CommandProbe, err = normalizeCommandProbe(*input.CommandProbe, workspace, rejectProbeSymlinks, stepWhere+".command_probe")
			if err != nil {
				return nil, err
			}
		}
		steps = append(steps, step)
	}
	for index, step := range steps {
		if step.InterpolatePort && !anyAutoPort {
			return nil, fmt.Errorf("%s[%d].args_utf8 uses ${TCP_PORT} without a tcp_probe.port: auto in this sequence", where, index)
		}
		if step.CommandProbe != nil && !anyAutoPort {
			for _, arg := range step.CommandProbe.Args {
				if arg == "${TCP_PORT}" {
					return nil, fmt.Errorf("%s[%d].command_probe.args uses ${TCP_PORT} without a tcp_probe.port: auto in this sequence", where, index)
				}
			}
		}
	}
	return steps, nil
}

func normalizeStepArguments(utf8Value, base64Value *string, where string) ([]byte, bool, error) {
	if utf8Value != nil && base64Value != nil {
		return nil, false, fmt.Errorf("%s cannot set both args_utf8 and args_base64", where)
	}
	if base64Value != nil {
		decoded, err := decodeBase64(*base64Value, where+".args_base64")
		return decoded, false, err
	}
	if utf8Value == nil {
		return nil, false, nil
	}
	value := *utf8Value
	if !utf8.ValidString(value) {
		return nil, false, fmt.Errorf("%s.args_utf8 must contain valid UTF-8", where)
	}
	if len(value) > maxInlineBytes {
		return nil, false, fmt.Errorf("%s.args_utf8 exceeds %d bytes", where, maxInlineBytes)
	}
	withoutPort := strings.ReplaceAll(value, "${TCP_PORT}", "")
	if strings.Contains(withoutPort, "${") {
		return nil, false, fmt.Errorf("%s.args_utf8 contains an unsupported ${...} placeholder", where)
	}
	return []byte(value), strings.Contains(value, "${TCP_PORT}"), nil
}

func normalizeTCPProbe(raw rawTCPProbe, where string) (*TCPProbe, error) {
	probe := &TCPProbe{State: raw.State, Timeout: 10 * time.Second}
	switch {
	case raw.Port.Kind == yaml.ScalarNode && raw.Port.Tag == "!!str" && raw.Port.Value == "auto":
		probe.AutoPort = true
	case raw.Port.Kind == yaml.ScalarNode && raw.Port.Tag == "!!int":
		var port int
		if err := raw.Port.Decode(&port); err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("%s.port must be an integer from 1 to 65535 or auto", where)
		}
		probe.Port = uint16(port)
	default:
		return nil, fmt.Errorf("%s.port must be an integer from 1 to 65535 or auto", where)
	}
	if probe.State != "open" && probe.State != "closed" {
		return nil, fmt.Errorf("%s.state must be open or closed", where)
	}
	if raw.Timeout != nil {
		duration, err := time.ParseDuration(*raw.Timeout)
		if err != nil {
			return nil, fmt.Errorf("%s.timeout: %w", where, err)
		}
		probe.Timeout = duration
	}
	if probe.Timeout <= 0 || probe.Timeout > maxProbeTimeout {
		return nil, fmt.Errorf("%s.timeout must be greater than zero and no more than %s", where, maxProbeTimeout)
	}
	if raw.BannerUTF8 != nil {
		if probe.State != "open" {
			return nil, fmt.Errorf("%s.banner_utf8 requires state: open", where)
		}
		if !utf8.ValidString(*raw.BannerUTF8) || len(*raw.BannerUTF8) > maxBannerBytes {
			return nil, fmt.Errorf("%s.banner_utf8 must be valid UTF-8 and no more than %d bytes", where, maxBannerBytes)
		}
		probe.Banner = []byte(*raw.BannerUTF8)
	}
	return probe, nil
}

func normalizeCommandProbe(raw rawCommandProbe, workspace pathResolver, rejectSymlinks bool, where string) (*CommandProbe, error) {
	if err := safepath.ValidatePortableRelative(raw.Path); err != nil {
		return nil, fmt.Errorf("%s.path: %w", where, err)
	}
	path := filepath.Join(workspace.root, filepath.FromSlash(raw.Path))
	if rejectSymlinks {
		if err := rejectSymlinkPath(workspace.root, path); err != nil {
			return nil, fmt.Errorf("%s.path: %w", where, err)
		}
	}
	resolved, err := workspace.file(path, where+".path")
	if err != nil {
		return nil, err
	}
	probe := &CommandProbe{Path: resolved, Timeout: 30 * time.Second}
	if len(raw.Args) > maxProbeArgs {
		return nil, fmt.Errorf("%s.args may contain at most %d arguments", where, maxProbeArgs)
	}
	for index, arg := range raw.Args {
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, '\x00') || len(arg) > maxProbeArgBytes {
			return nil, fmt.Errorf("%s.args[%d] must be valid UTF-8 without NUL and no more than %d bytes", where, index, maxProbeArgBytes)
		}
		if strings.Contains(arg, "${") && arg != "${TCP_PORT}" {
			return nil, fmt.Errorf("%s.args[%d] contains an unsupported or partial ${...} placeholder", where, index)
		}
		probe.Args = append(probe.Args, arg)
	}
	if raw.Timeout != nil {
		duration, err := time.ParseDuration(*raw.Timeout)
		if err != nil {
			return nil, fmt.Errorf("%s.timeout: %w", where, err)
		}
		probe.Timeout = duration
	}
	if probe.Timeout <= 0 || probe.Timeout > maxProbeTimeout {
		return nil, fmt.Errorf("%s.timeout must be greater than zero and no more than %s", where, maxProbeTimeout)
	}
	return probe, nil
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
	output.Expect = cloneExpectation(input.Expect)
	output.Modes = append([]string(nil), input.Modes...)
	output.Arguments = append([]byte(nil), input.Arguments...)
	return output
}

func cloneExpectation(input Expectation) Expectation {
	output := input
	if input.Output.Equals != nil {
		equals := *input.Output.Equals
		output.Output.Equals = &equals
	}
	output.Output.Contains = append([]string(nil), input.Output.Contains...)
	output.Output.NotContains = append([]string(nil), input.Output.NotContains...)
	output.Output.Matches = append([]string(nil), input.Output.Matches...)
	output.Output.EqualsBytes = append([]byte(nil), input.Output.EqualsBytes...)
	return output
}

func cloneSteps(input []Step) []Step {
	if input == nil {
		return nil
	}
	output := make([]Step, len(input))
	for index, step := range input {
		output[index] = step
		output[index].Arguments = append([]byte(nil), step.Arguments...)
		output[index].Expect = cloneExpectation(step.Expect)
		if step.TCPProbe != nil {
			probe := *step.TCPProbe
			probe.Banner = append([]byte(nil), step.TCPProbe.Banner...)
			output[index].TCPProbe = &probe
		}
		if step.CommandProbe != nil {
			probe := *step.CommandProbe
			probe.Args = append([]string(nil), step.CommandProbe.Args...)
			output[index].CommandProbe = &probe
		}
	}
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
