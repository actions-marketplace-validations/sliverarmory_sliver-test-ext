package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/sliverarmory/sliver-test-ext/internal/acquire"
	"github.com/sliverarmory/sliver-test-ext/internal/harness"
	"github.com/sliverarmory/sliver-test-ext/internal/manifest"
	"github.com/sliverarmory/sliver-test-ext/internal/results"
)

const (
	defaultManifest       = ".github/sliver-ext-e2e.yml"
	planSchema            = "sliver-ext-e2e-plan/v1"
	preparedSchema        = "sliver-ext-e2e-prepared/v1"
	maxPreparedDescriptor = 1 << 20
)

type commonFlags struct {
	manifestPath  string
	workspace     string
	libraryRoot   string
	resultsDir    string
	githubOutput  string
	sliverVersion string
	actionVersion string
}

type planDocument struct {
	Schema     string             `json:"schema"`
	Manifest   string             `json:"manifest"`
	Matrix     manifest.Matrix    `json:"matrix"`
	Libraries  []plannedLibrary   `json:"libraries"`
	Resolution acquire.Resolution `json:"sliver"`
}

type plannedLibrary struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type aggregateDocument struct {
	Schema  string           `json:"schema"`
	Reports []results.Report `json:"reports"`
}

// preparedDocument is a private, short-lived handoff from acquisition to
// execution. It never contains the credential used to acquire Sliver.
type preparedDocument struct {
	Schema     string             `json:"schema"`
	Target     string             `json:"target"`
	Selector   string             `json:"selector"`
	ServerPath string             `json:"server_path"`
	Sliver     acquire.Provenance `json:"sliver"`
}

type stderrLogger struct{ logger *log.Logger }

func (logger stderrLogger) Printf(format string, args ...any) {
	logger.logger.Printf(format, args...)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "sliver-ext-e2e: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected a command: plan, prepare, run, or aggregate")
	}
	switch args[0] {
	case "plan":
		return runPlan(ctx, args[1:])
	case "prepare":
		return runPrepare(ctx, args[1:])
	case "run":
		return runTarget(ctx, args[1:])
	case "aggregate":
		return runAggregate(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q; expected plan, prepare, run, or aggregate", args[0])
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: sliver-ext-e2e <plan|prepare|run|aggregate> [options]")
}

func addCommonFlags(flags *flag.FlagSet, includeVersion bool) *commonFlags {
	values := &commonFlags{}
	flags.StringVar(&values.manifestPath, "manifest", defaultManifest, "path to the test manifest")
	flags.StringVar(&values.workspace, "workspace", "", "caller workspace (defaults to current directory)")
	flags.StringVar(&values.libraryRoot, "library-root", "", "optional isolated root for planner-staged extension libraries")
	flags.StringVar(&values.resultsDir, "results-dir", ".sliver-ext-e2e/results", "result directory")
	flags.StringVar(&values.githubOutput, "github-output", "", "GitHub Actions output command file")
	flags.StringVar(&values.actionVersion, "action-version", "", "immutable action/workflow identity recorded in reports")
	if includeVersion {
		flags.StringVar(&values.sliverVersion, "sliver-version", "", "Sliver selector override")
	}
	return values
}

func (values *commonFlags) normalize() error {
	var err error
	if strings.TrimSpace(values.workspace) == "" {
		values.workspace, err = os.Getwd()
	} else {
		values.workspace, err = filepath.Abs(values.workspace)
	}
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	values.workspace, err = filepath.EvalSymlinks(values.workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	if !filepath.IsAbs(values.manifestPath) {
		values.manifestPath = filepath.Join(values.workspace, values.manifestPath)
	}
	if !filepath.IsAbs(values.resultsDir) {
		values.resultsDir = filepath.Join(values.workspace, values.resultsDir)
	}
	if strings.TrimSpace(values.libraryRoot) != "" {
		if !filepath.IsAbs(values.libraryRoot) {
			values.libraryRoot = filepath.Join(values.workspace, values.libraryRoot)
		}
		values.libraryRoot = filepath.Clean(values.libraryRoot)
	}
	values.manifestPath = filepath.Clean(values.manifestPath)
	values.resultsDir = filepath.Clean(values.resultsDir)
	values.actionVersion, err = resolveActionVersion(values.actionVersion)
	if err != nil {
		return err
	}
	return nil
}

func runPlan(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags, true)
	var librariesDir string
	flags.StringVar(&librariesDir, "libraries-dir", "", "private directory in which to stage validated extension libraries")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("plan: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := common.normalize(); err != nil {
		return err
	}
	loaded, err := manifest.LoadForPlanning(common.manifestPath, common.workspace)
	if err != nil {
		return err
	}
	selector := selectedSliverVersion(common.sliverVersion, loaded.Config.Sliver.Version)
	resolution, err := acquire.Resolve(ctx, selector, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return err
	}
	matrix := loaded.Matrix()
	if len(matrix.Include) == 0 {
		return errors.New("manifest produced an empty target matrix")
	}
	libraries, err := stagePlanLibraries(loaded.Workspace, librariesDir, loaded.Plans)
	if err != nil {
		return fmt.Errorf("stage planned extension libraries: %w", err)
	}
	document := planDocument{Schema: planSchema, Manifest: loaded.Path, Matrix: matrix, Libraries: libraries, Resolution: resolution}
	if err := writeJSON(filepath.Join(common.resultsDir, "plan.json"), document); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	matrixJSON, err := json.Marshal(matrix)
	if err != nil {
		return err
	}
	outputs := map[string]string{
		"matrix":          string(matrixJSON),
		"resolved-sliver": resolution.Selector,
		"results-dir":     common.resultsDir,
	}
	if strings.TrimSpace(librariesDir) != "" {
		absoluteLibrariesDir, absoluteErr := filepath.Abs(librariesDir)
		if absoluteErr != nil {
			return fmt.Errorf("resolve staged libraries directory: %w", absoluteErr)
		}
		outputs["libraries-dir"] = filepath.Clean(absoluteLibrariesDir)
	}
	if err := appendOutputs(common.githubOutput, outputs); err != nil {
		return err
	}
	fmt.Printf("Planned %d target(s) with Sliver %s\n", len(matrix.Include), resolution.Selector)
	return nil
}

func runPrepare(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags, true)
	var targetKey, serverPath, preparedPath string
	flags.StringVar(&targetKey, "target", "", "target os/arch")
	flags.StringVar(&serverPath, "server-path", "", "existing sliver-server binary")
	flags.StringVar(&preparedPath, "prepared", "", "private prepared descriptor to create")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("prepare: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(preparedPath) == "" {
		return errors.New("prepare: --prepared is required")
	}
	if err := common.normalize(); err != nil {
		return err
	}
	loaded, err := loadManifest(common)
	if err != nil {
		return err
	}
	targetKey, err = chooseTarget(targetKey, loaded.Matrix())
	if err != nil {
		return err
	}
	if len(loaded.PlansForTarget(targetKey)) == 0 {
		return fmt.Errorf("manifest has no plans for target %q", targetKey)
	}
	selector := selectedSliverVersion(common.sliverVersion, loaded.Config.Sliver.Version)
	if serverPath != "" && !filepath.IsAbs(serverPath) {
		serverPath = filepath.Join(common.workspace, serverPath)
	}
	preparedPath, err = auxiliaryPath(preparedPath, common.workspace)
	if err != nil {
		return err
	}
	cacheDir, err := toolCacheDir()
	if err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, err)
	}
	logger := stderrLogger{logger: log.New(os.Stderr, "", log.LstdFlags)}
	server, provenance, err := acquire.Server(ctx, acquire.Options{
		Version: selector, ServerPath: serverPath, CacheDir: cacheDir,
		Token: os.Getenv("GITHUB_TOKEN"), Logger: logger,
	})
	if err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, err)
	}
	document := preparedDocument{
		Schema: preparedSchema, Target: targetKey, Selector: selector,
		ServerPath: server, Sliver: provenance,
	}
	if err := writePreparedDescriptor(preparedPath, document); err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, provenance, fmt.Errorf("write prepared descriptor: %w", err))
	}
	resolved := resolvedSelector(provenance)
	if err := appendOutputs(common.githubOutput, map[string]string{
		"resolved-sliver": resolved,
		"results-dir":     common.resultsDir,
		"target":          targetKey,
	}); err != nil {
		return err
	}
	fmt.Printf("Prepared Sliver %s for %s\n", resolved, targetKey)
	return nil
}

func runTarget(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags, true)
	var targetKey, stateFile, preparedPath string
	flags.StringVar(&targetKey, "target", "", "target os/arch")
	flags.StringVar(&stateFile, "state-file", "", "cleanup state file")
	flags.StringVar(&preparedPath, "prepared", "", "private descriptor created by prepare")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("run: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(preparedPath) == "" {
		return errors.New("run: --prepared is required; acquire Sliver with prepare first")
	}
	if err := common.normalize(); err != nil {
		return err
	}
	// Execution never performs network acquisition. Remove an ambient token
	// before parsing caller-controlled inputs or launching any Sliver process,
	// even for direct CLI users outside the composite action.
	_ = os.Unsetenv("GITHUB_TOKEN")
	loaded, err := loadManifest(common)
	if err != nil {
		return err
	}
	targetKey, err = chooseTarget(targetKey, loaded.Matrix())
	if err != nil {
		return err
	}
	plans := loaded.PlansForTarget(targetKey)
	if len(plans) == 0 {
		return fmt.Errorf("manifest has no plans for target %q", targetKey)
	}
	target, err := parseTarget(targetKey)
	if err != nil {
		return err
	}
	selector := selectedSliverVersion(common.sliverVersion, loaded.Config.Sliver.Version)
	cases, err := convertPlans(plans)
	if err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, err)
	}
	if stateFile == "" {
		stateFile = filepath.Join(common.resultsDir, "cleanup-state.json")
	} else if !filepath.IsAbs(stateFile) {
		stateFile = filepath.Join(common.workspace, stateFile)
	}
	preparedPath, err = auxiliaryPath(preparedPath, common.workspace)
	if err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, err)
	}
	prepared, preparedErr := readPreparedDescriptor(preparedPath, targetKey, selector)
	if preparedErr != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, fmt.Errorf("read prepared descriptor: %w", preparedErr))
	}
	server, provenance := prepared.ServerPath, prepared.Sliver
	report, runErr := harness.Run(ctx, harness.Options{
		Workspace: common.workspace, ServerPath: server, Target: target, Cases: cases,
		ResultsDir: common.resultsDir, StateFile: stateFile,
		Sliver: toResultProvenance(provenance), ActionVersion: common.actionVersion,
	})
	if report == nil {
		return errors.Join(runErr, errors.New("harness returned no report"))
	}
	writeErr := report.Write(common.resultsDir)
	summaryErr := appendSummary(report.Markdown())
	resolved := resolvedSelector(provenance)
	outputErr := appendOutputs(common.githubOutput, map[string]string{
		"resolved-sliver": resolved,
		"results-dir":     common.resultsDir,
	})
	return errors.Join(runErr, writeErr, summaryErr, outputErr)
}

func writeInfrastructureFailure(common *commonFlags, target, selector string, provenance acquire.Provenance, cause error) error {
	report := results.New(target)
	report.ActionVersion = common.actionVersion
	report.InfrastructureError = cause.Error()
	if provenance.Selector == "" {
		provenance.Selector = selector
	}
	report.Sliver = toResultProvenance(provenance)
	writeErr := report.Write(common.resultsDir)
	summaryErr := appendSummary(report.Markdown())
	resolved := resolvedSelector(provenance)
	if resolved == "" {
		resolved = selector
	}
	outputErr := appendOutputs(common.githubOutput, map[string]string{
		"resolved-sliver": resolved,
		"results-dir":     common.resultsDir,
	})
	return errors.Join(cause, writeErr, summaryErr, outputErr)
}

func runAggregate(args []string) error {
	flags := flag.NewFlagSet("aggregate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags, false)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("aggregate: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := common.normalize(); err != nil {
		return err
	}
	loaded, err := loadManifest(common)
	if err != nil {
		return err
	}
	reports, summary, aggregateErr := results.Aggregate(common.resultsDir)
	coverageErr := verifyTargetCoverage(loaded.Matrix(), reports)
	caseCoverageErr := verifyCaseCoverage(loaded.Plans, reports)
	provenanceErr := verifyReportProvenance(reports)
	if coverageErr != nil {
		summary += "\nMatrix completeness: **FAIL** — " + coverageErr.Error() + "\n"
	} else {
		summary += "\nMatrix completeness: **PASS**\n"
	}
	if caseCoverageErr != nil {
		summary += "\nCase completeness: **FAIL** — " + caseCoverageErr.Error() + "\n"
	} else {
		summary += "\nCase completeness: **PASS**\n"
	}
	if provenanceErr != nil {
		summary += "\nProvenance consistency: **FAIL** — " + provenanceErr.Error() + "\n"
	} else {
		summary += "\nProvenance consistency: **PASS**\n"
	}
	writeJSONErr := writeJSON(filepath.Join(common.resultsDir, "aggregate.json"), aggregateDocument{
		Schema: "sliver-ext-e2e-aggregate/v1", Reports: reports,
	})
	writeSummaryErr := os.WriteFile(filepath.Join(common.resultsDir, "aggregate.md"), []byte(summary), 0o600)
	stepSummaryErr := appendSummary(summary)
	outputErr := appendOutputs(common.githubOutput, map[string]string{"results-dir": common.resultsDir})
	return errors.Join(aggregateErr, coverageErr, caseCoverageErr, provenanceErr, writeJSONErr, writeSummaryErr, stepSummaryErr, outputErr)
}

func loadManifest(common *commonFlags) (*manifest.Manifest, error) {
	if strings.TrimSpace(common.libraryRoot) != "" {
		return manifest.LoadWithLibraryRoot(common.manifestPath, common.workspace, common.libraryRoot)
	}
	return manifest.Load(common.manifestPath, common.workspace)
}

func selectedSliverVersion(override, configured string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	return strings.TrimSpace(configured)
}

func chooseTarget(requested string, matrix manifest.Matrix) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(matrix.Include) == 1 {
			return matrix.Include[0].Target, nil
		}
		return "", fmt.Errorf("--target is required when the manifest contains %d targets", len(matrix.Include))
	}
	for _, entry := range matrix.Include {
		if entry.Target == requested {
			return requested, nil
		}
	}
	return "", fmt.Errorf("target %q is not present in the manifest", requested)
}

func parseTarget(value string) (harness.Target, error) {
	osName, architecture, ok := strings.Cut(value, "/")
	if !ok || osName == "" || architecture == "" || strings.Contains(architecture, "/") {
		return harness.Target{}, fmt.Errorf("invalid target %q; expected os/arch", value)
	}
	return harness.Target{OS: osName, Arch: architecture}, nil
}

func convertPlans(plans []manifest.Plan) ([]harness.Case, error) {
	cases := make([]harness.Case, 0, len(plans))
	for _, plan := range plans {
		library, err := os.ReadFile(plan.Library)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", plan.ID, err)
		}
		digest := sha256.Sum256(library)
		steps := make([]harness.Step, 0, len(plan.Steps))
		for _, step := range plan.Steps {
			converted := harness.Step{
				Name: step.Name, Export: step.Export,
				Arguments: append([]byte(nil), step.Arguments...), InterpolatePort: step.InterpolatePort,
				CommandTimeout: step.CommandTimeout, Expectation: convertExpectation(step.Expect),
			}
			if step.TCPProbe != nil {
				converted.TCPProbe = &harness.TCPProbe{
					Port: step.TCPProbe.Port, AutoPort: step.TCPProbe.AutoPort,
					State: step.TCPProbe.State, Timeout: step.TCPProbe.Timeout,
					Banner: append([]byte(nil), step.TCPProbe.Banner...),
				}
			}
			if step.CommandProbe != nil {
				converted.CommandProbe = &harness.CommandProbe{
					Path: step.CommandProbe.Path, Args: append([]string(nil), step.CommandProbe.Args...),
					Timeout: step.CommandProbe.Timeout,
				}
			}
			steps = append(steps, converted)
		}
		cases = append(cases, harness.Case{
			ID: plan.ID, Suite: plan.Suite, Name: plan.Name, Collection: plan.Collection,
			Target:      harness.Target{OS: plan.Target.OS, Arch: plan.Target.Arch},
			LibraryPath: plan.Library, LibraryReportPath: plan.LibraryPath,
			LibraryData: library, LibrarySHA256: hex.EncodeToString(digest[:]),
			Export: plan.Settings.Export, Init: plan.Settings.Init,
			Modes:  append([]string(nil), plan.Settings.Modes...),
			Repeat: plan.Settings.Repeat, CommandTimeout: plan.Settings.CommandTimeout,
			Arguments:   append([]byte(nil), plan.Settings.Arguments...),
			Expectation: convertExpectation(plan.Settings.Expect), Steps: steps,
		})
	}
	return cases, nil
}

func convertExpectation(input manifest.Expectation) harness.Expectation {
	output := harness.OutputExpectation{
		Equals: input.Output.Equals, Contains: append([]string(nil), input.Output.Contains...),
		NotContains: append([]string(nil), input.Output.NotContains...),
		Matches:     append([]string(nil), input.Output.Matches...), SHA256: input.Output.SHA256,
	}
	if input.Output.HasEqualsB64 {
		output.EqualsBase64 = base64.StdEncoding.EncodeToString(input.Output.EqualsBytes)
		output.HasEqualsBase64 = true
	}
	return harness.Expectation{SuccessOnly: input.SuccessOnly, Output: output}
}

func auxiliaryPath(value, workspace string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(workspace, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func writePreparedDescriptor(path string, document preparedDocument) error {
	if document.Schema != preparedSchema {
		return fmt.Errorf("prepared descriptor schema must be %q", preparedSchema)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("prepared descriptor already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxPreparedDescriptor {
		return fmt.Errorf("prepared descriptor exceeds %d bytes", maxPreparedDescriptor)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".prepared-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func readPreparedDescriptor(path, target, selector string) (preparedDocument, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return preparedDocument{}, err
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maxPreparedDescriptor {
		return preparedDocument{}, errors.New("prepared descriptor is not a bounded regular file")
	}
	if runtime.GOOS != "windows" && before.Mode().Perm()&0o077 != 0 {
		return preparedDocument{}, errors.New("prepared descriptor must not be accessible by group or other users")
	}
	file, err := os.Open(path)
	if err != nil {
		return preparedDocument{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return preparedDocument{}, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() != before.Size() {
		return preparedDocument{}, errors.New("prepared descriptor changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxPreparedDescriptor+1))
	decoder.DisallowUnknownFields()
	document := preparedDocument{}
	if err := decoder.Decode(&document); err != nil {
		return preparedDocument{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return preparedDocument{}, errors.New("prepared descriptor contains trailing JSON data")
	}
	if document.Schema != preparedSchema {
		return preparedDocument{}, fmt.Errorf("unsupported prepared descriptor schema %q", document.Schema)
	}
	if document.Target != target {
		return preparedDocument{}, fmt.Errorf("prepared target %q does not match requested target %q", document.Target, target)
	}
	if document.Selector != selector {
		return preparedDocument{}, fmt.Errorf("prepared selector %q does not match requested selector %q", document.Selector, selector)
	}
	if !filepath.IsAbs(document.ServerPath) {
		return preparedDocument{}, errors.New("prepared server path is not absolute")
	}
	if err := verifyPreparedFile(document.ServerPath, document.Sliver.Digest); err != nil {
		return preparedDocument{}, fmt.Errorf("verify prepared Sliver server: %w", err)
	}
	return document, nil
}

func verifyPreparedFile(path, wantDigest string) error {
	if strings.TrimSpace(wantDigest) == "" {
		return errors.New("prepared provenance has no SHA-256")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("prepared server is not a regular file")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != strings.ToLower(wantDigest) {
		return fmt.Errorf("SHA-256 %s does not match prepared provenance %s", actual, wantDigest)
	}
	return nil
}

func toolCacheDir() (string, error) {
	// RUNNER_TOOL_CACHE can be shared by unrelated jobs on a self-hosted runner.
	// Keep executable cache entries inside the current job's private temp root.
	if root := strings.TrimSpace(os.Getenv("RUNNER_TEMP")); root != "" {
		return filepath.Join(root, "sliver-ext-e2e-cache"), nil
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache: %w", err)
	}
	return filepath.Join(root, "sliver-ext-e2e"), nil
}

func toResultProvenance(value acquire.Provenance) results.Provenance {
	resolved := ""
	if value.ResolvedTag != "" {
		resolved = value.ResolvedTag
	} else if value.ResolvedSHA != "" {
		resolved = value.ResolvedSHA
	}
	return results.Provenance{
		Selector: value.Selector, Resolved: resolved, ResolvedTag: value.ResolvedTag,
		ResolvedSHA: value.ResolvedSHA, SHA256: value.Digest, SourceBuilt: value.SourceBuilt,
		CacheHit: value.CacheHit, Verified: value.Verified, Source: value.Source, Toolchain: value.Toolchain,
	}
}

func resolvedSelector(value acquire.Provenance) string {
	if value.Selector != "" && value.Selector != "path" {
		return value.Selector
	}
	if value.ResolvedSHA != "" {
		return value.ResolvedSHA
	}
	return value.ResolvedTag
}

func resolveActionVersion(override string) (string, error) {
	value := strings.TrimSpace(override)
	if value == "" {
		value = strings.TrimSpace(os.Getenv("SLIVER_EXT_E2E_ACTION_VERSION"))
	}
	if value == "" {
		// The Linux/386 container action uses this shorter environment name at
		// its explicit container boundary.
		value = strings.TrimSpace(os.Getenv("SLIVER_EXT_ACTION_VERSION"))
	}
	repository := strings.TrimSpace(os.Getenv("GITHUB_ACTION_REPOSITORY"))
	reference := strings.TrimSpace(os.Getenv("GITHUB_ACTION_REF"))
	if value == "" && repository != "" && reference != "" {
		value = repository + "@" + reference
	}
	if value == "" && repository != "" {
		value = repository
	}
	if value == "" {
		value = "development"
	}
	if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("action version must be a single line of at most 512 bytes")
	}
	return value, nil
}

func verifyTargetCoverage(matrix manifest.Matrix, reports []results.Report) error {
	expected := make(map[string]struct{}, len(matrix.Include))
	for _, entry := range matrix.Include {
		expected[entry.Target] = struct{}{}
	}
	seen := make(map[string]struct{}, len(reports))
	var problems []string
	for _, report := range reports {
		if _, duplicate := seen[report.Target]; duplicate {
			problems = append(problems, "duplicate report for "+report.Target)
			continue
		}
		seen[report.Target] = struct{}{}
		if _, wanted := expected[report.Target]; !wanted {
			problems = append(problems, "unexpected report for "+report.Target)
		}
	}
	for target := range expected {
		if _, ok := seen[target]; !ok {
			problems = append(problems, "missing report for "+target)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

type executionKey struct {
	target string
	id     string
	mode   string
	repeat int
	step   int
}

type expectedExecution struct {
	suite         string
	test          string
	collection    string
	library       string
	librarySHA256 string
	export        string
	init          string
	stepName      string
}

func verifyCaseCoverage(plans []manifest.Plan, reports []results.Report) error {
	expected := make(map[executionKey]expectedExecution)
	validModes := make(map[executionKey]struct{})
	var problems []string
	for _, plan := range plans {
		library, err := os.ReadFile(plan.Library)
		if err != nil {
			problems = append(problems, fmt.Sprintf("read planned library %s: %v", plan.ID, err))
			continue
		}
		digest := sha256.Sum256(library)
		metadata := expectedExecution{
			suite: plan.Suite, test: plan.Name, collection: plan.Collection, library: plan.LibraryPath,
			librarySHA256: hex.EncodeToString(digest[:]),
			export:        plan.Settings.Export, init: plan.Settings.Init,
		}
		for _, mode := range plan.Settings.Modes {
			validModes[executionKey{target: plan.Target.Key(), id: plan.ID, mode: mode}] = struct{}{}
			for repeat := 1; repeat <= plan.Settings.Repeat; repeat++ {
				if len(plan.Steps) == 0 {
					expected[executionKey{target: plan.Target.Key(), id: plan.ID, mode: mode, repeat: repeat}] = metadata
					continue
				}
				for index, step := range plan.Steps {
					stepMetadata := metadata
					stepMetadata.export = step.Export
					stepMetadata.stepName = step.Name
					expected[executionKey{target: plan.Target.Key(), id: plan.ID, mode: mode, repeat: repeat, step: index + 1}] = stepMetadata
				}
			}
		}
	}
	seen := make(map[executionKey]struct{})
	for _, report := range reports {
		for _, result := range report.Cases {
			if result.Repeat == 0 && strings.HasPrefix(result.Stage, "cleanup-") && result.Status != "pass" {
				if _, ok := validModes[executionKey{target: report.Target, id: result.ID, mode: result.Mode}]; !ok {
					problems = append(problems, fmt.Sprintf("unexpected cleanup result %s/%s", report.Target, result.ID))
				}
				continue
			}
			key := executionKey{target: report.Target, id: result.ID, mode: result.Mode, repeat: result.Repeat, step: result.StepIndex}
			metadata, ok := expected[key]
			if !ok {
				problems = append(problems, "unexpected case "+formatExecutionKey(key))
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				problems = append(problems, "duplicate case "+formatExecutionKey(key))
				continue
			}
			seen[key] = struct{}{}
			if result.Suite != metadata.suite || result.Test != metadata.test || result.Collection != metadata.collection || result.Library != metadata.library || result.LibrarySHA256 != metadata.librarySHA256 || result.Export != metadata.export || result.Init != metadata.init || result.Step != metadata.stepName {
				problems = append(problems, "metadata mismatch for "+formatExecutionKey(key))
			}
		}
	}
	for key := range expected {
		if _, ok := seen[key]; !ok {
			problems = append(problems, "missing case "+formatExecutionKey(key))
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func formatExecutionKey(key executionKey) string {
	identifier := fmt.Sprintf("%s/%s/%s#%d", key.target, key.id, key.mode, key.repeat)
	if key.step != 0 {
		return fmt.Sprintf("%s/step-%d", identifier, key.step)
	}
	return identifier
}

func verifyReportProvenance(reports []results.Report) error {
	if len(reports) == 0 {
		return errors.New("no target provenance to compare")
	}
	wantSHA := reports[0].Sliver.ResolvedSHA
	wantSelector := reports[0].Sliver.Selector
	wantAction := reports[0].ActionVersion
	var problems []string
	for _, report := range reports {
		if report.Sliver.ResolvedSHA == "" {
			problems = append(problems, "missing resolved Sliver SHA for "+report.Target)
		} else if wantSHA != "" && report.Sliver.ResolvedSHA != wantSHA {
			problems = append(problems, "Sliver SHA mismatch for "+report.Target)
		}
		if report.Sliver.Selector == "" {
			problems = append(problems, "missing Sliver selector for "+report.Target)
		} else if wantSelector != "" && report.Sliver.Selector != wantSelector {
			problems = append(problems, "Sliver selector mismatch for "+report.Target)
		}
		if report.ActionVersion == "" {
			problems = append(problems, "missing action version for "+report.Target)
		} else if wantAction != "" && report.ActionVersion != wantAction {
			problems = append(problems, "action version mismatch for "+report.Target)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func appendOutputs(path string, values map[string]string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open GitHub output file: %w", err)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := values[key]
		if strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(value, "\r\n") {
			_ = file.Close()
			return fmt.Errorf("GitHub output %q contains a newline", key)
		}
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}

func appendSummary(summary string) error {
	path := strings.TrimSpace(os.Getenv("GITHUB_STEP_SUMMARY"))
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(summary, "\n") {
		summary += "\n"
	}
	_, writeErr := file.WriteString(summary)
	return errors.Join(writeErr, file.Close())
}
