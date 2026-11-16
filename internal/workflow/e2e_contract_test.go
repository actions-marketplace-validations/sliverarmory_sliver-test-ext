package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	uploadArtifactRef   = "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a"
	downloadArtifactRef = "actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c"
)

type workflowFile struct {
	On struct {
		WorkflowCall struct {
			Inputs map[string]workflowInput `yaml:"inputs"`
		} `yaml:"workflow_call"`
	} `yaml:"on"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowInput struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
	Type        string `yaml:"type"`
	Default     string `yaml:"default"`
}

type workflowJob struct {
	Steps []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name             string         `yaml:"name"`
	Uses             string         `yaml:"uses"`
	If               string         `yaml:"if"`
	Shell            string         `yaml:"shell"`
	WorkingDirectory string         `yaml:"working-directory"`
	Run              string         `yaml:"run"`
	Env              map[string]any `yaml:"env"`
	With             map[string]any `yaml:"with"`
}

func TestReusableWorkflowBuildsOnceAndDistributesValidatedLibraries(t *testing.T) {
	workflow := readWorkflow(t)
	input, ok := workflow.On.WorkflowCall.Inputs["prepare-command"]
	if !ok {
		t.Fatal("workflow_call input prepare-command is missing")
	}
	if input.Type != "string" || input.Required || input.Default != "" {
		t.Fatalf("prepare-command input = %#v", input)
	}
	if !strings.Contains(strings.ToLower(input.Description), "trusted") || !strings.Contains(strings.ToLower(input.Description), "all") {
		t.Fatalf("prepare-command description does not state its trust/all-target contract: %q", input.Description)
	}

	plan := workflow.Jobs["plan"].Steps
	checkout := stepIndex(t, plan, "Check out extension repository")
	setupGo := stepIndex(t, plan, "Set up Go")
	prepare := stepIndex(t, plan, "Prepare caller extension libraries")
	validate := stepIndex(t, plan, "Validate manifest and plan matrix")
	upload := stepIndex(t, plan, "Upload validated extension libraries")
	if !(checkout < setupGo && setupGo < prepare && prepare < validate && validate < upload) {
		t.Fatalf("plan step order checkout=%d setup=%d prepare=%d validate=%d upload=%d", checkout, setupGo, prepare, validate, upload)
	}
	prepareStep := plan[prepare]
	if prepareStep.If != "${{ inputs.prepare-command != '' }}" || prepareStep.Uses != "$/.github/actions/prepare-caller" || scalar(prepareStep.With["command"]) != "${{ inputs.prepare-command }}" {
		t.Fatalf("prepare step contract = %#v", prepareStep)
	}
	uploadStep := plan[upload]
	if uploadStep.Uses != uploadArtifactRef || scalar(uploadStep.With["name"]) != "sliver-ext-e2e-libraries" || scalar(uploadStep.With["path"]) != "${{ steps.plan.outputs.libraries-dir }}" || scalar(uploadStep.With["if-no-files-found"]) != "error" || scalar(uploadStep.With["include-hidden-files"]) != "true" || scalar(uploadStep.With["retention-days"]) != "7" || scalar(uploadStep.With["overwrite"]) != "true" {
		t.Fatalf("library upload contract = %#v", uploadStep)
	}

	for _, jobName := range []string{"run", "aggregate"} {
		steps := workflow.Jobs[jobName].Steps
		if countSteps(steps, "Prepare caller extension libraries") != 0 {
			t.Fatalf("%s unexpectedly reruns prepare-command", jobName)
		}
		restore := stepIndex(t, steps, "Restore validated extension libraries")
		checkout := stepIndex(t, steps, "Check out extension repository")
		if checkout >= restore {
			t.Fatalf("%s restore occurs before checkout", jobName)
		}
		step := steps[restore]
		if step.Uses != downloadArtifactRef || scalar(step.With["name"]) != "sliver-ext-e2e-libraries" || scalar(step.With["path"]) != "${{ runner.temp }}/sliver-ext-e2e-libraries" {
			t.Fatalf("%s restore contract = %#v", jobName, step)
		}
	}
	if restore := stepIndex(t, workflow.Jobs["run"].Steps, "Restore validated extension libraries"); restore >= stepIndex(t, workflow.Jobs["run"].Steps, "Run Sliver extension tests") {
		t.Fatal("run job restores libraries after target execution")
	}
	if restore := stepIndex(t, workflow.Jobs["run"].Steps, "Restore validated extension libraries"); restore >= stepIndex(t, workflow.Jobs["run"].Steps, "Run Sliver extension tests in a Linux/386 userspace") {
		t.Fatal("run job restores libraries after Linux/386 target execution")
	}
	if restore := stepIndex(t, workflow.Jobs["aggregate"].Steps, "Restore validated extension libraries"); restore >= stepIndex(t, workflow.Jobs["aggregate"].Steps, "Aggregate and verify results") {
		t.Fatal("aggregate job restores libraries after aggregation")
	}
	for _, name := range []string{"Run Sliver extension tests", "Run Sliver extension tests in a Linux/386 userspace"} {
		step := workflow.Jobs["run"].Steps[stepIndex(t, workflow.Jobs["run"].Steps, name)]
		if scalar(step.With["library-root"]) != "${{ runner.temp }}/sliver-ext-e2e-libraries" {
			t.Fatalf("%s does not consume the isolated library root: %#v", name, step.With)
		}
	}
	aggregate := workflow.Jobs["aggregate"].Steps[stepIndex(t, workflow.Jobs["aggregate"].Steps, "Aggregate and verify results")]
	if scalar(aggregate.With["library-root"]) != "${{ runner.temp }}/sliver-ext-e2e-libraries" {
		t.Fatalf("aggregate does not consume the isolated library root: %#v", aggregate.With)
	}
}

func TestLibraryRootPropagatesThroughEveryExecutionBoundary(t *testing.T) {
	contracts := map[string][]string{
		".github/actions/prepare/action.yml": {
			"library-root:", "Optional private root containing only planner-staged extension libraries.",
		},
		".github/actions/run/action.yml": {
			"library-root: ${{ inputs.library-root }}", "SLIVER_EXT_LIBRARY_ROOT: ${{ inputs.library-root }}",
		},
		".github/actions/run-linux-386/action.yml": {
			"SLIVER_EXT_LIBRARY_ROOT: ${{ inputs.library-root }}",
		},
		".github/actions/run-linux-386/run.sh": {
			`--env "SLIVER_EXT_LIBRARY_ROOT=${SLIVER_EXT_LIBRARY_ROOT}"`,
		},
		".github/actions/aggregate/action.yml": {
			"library-root:", "Optional private root containing only planner-staged extension libraries.",
		},
		"dist/index.js": {
			"const libraryRootInput = input('library-root');", "...(libraryRoot ? ['--library-root', libraryRoot] : []),",
		},
		"dist/run-prepared.sh": {
			`--library-root "${SLIVER_EXT_LIBRARY_ROOT:-}"`,
		},
	}
	for relative, fragments := range contracts {
		contents := readRepositoryFile(t, relative)
		for _, fragment := range fragments {
			if !strings.Contains(contents, fragment) {
				t.Errorf("%s is missing library-root handoff %q", relative, fragment)
			}
		}
	}
	entrypoint := readRepositoryFile(t, ".github/actions/run-linux-386/entrypoint.sh")
	if count := strings.Count(entrypoint, `--library-root "${SLIVER_EXT_LIBRARY_ROOT}"`); count != 2 {
		t.Fatalf("Linux/386 entrypoint library-root handoffs = %d, want prepare and run", count)
	}
}

func TestLinux386RouteKeepsTrustedToolsOnHostArchitecture(t *testing.T) {
	dockerfile := readRepositoryFile(t, ".github/actions/run-linux-386/Dockerfile")
	for _, fragment := range []string{
		"FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm",
		"CGO_ENABLED=0 GOOS=linux GOARCH=amd64",
		"FROM --platform=$TARGETPLATFORM debian:bookworm-slim",
	} {
		if !strings.Contains(dockerfile, fragment) {
			t.Errorf("Linux/386 Dockerfile is missing host/target architecture contract %q", fragment)
		}
	}

	runScript := readRepositoryFile(t, ".github/actions/run-linux-386/run.sh")
	if !strings.Contains(runScript, "--platform linux/386") {
		t.Fatal("Linux/386 route does not start an i386 userspace")
	}
	entrypoint := readRepositoryFile(t, ".github/actions/run-linux-386/entrypoint.sh")
	if !strings.Contains(entrypoint, `--target "${SLIVER_EXT_TARGET}"`) {
		t.Fatal("Linux/386 target is not passed as an implant/test selection")
	}
	for _, forbidden := range []string{"GOOS=", "GOARCH=", "CGO_ENABLED=", "CC="} {
		if strings.Contains(entrypoint, forbidden) {
			t.Errorf("Linux/386 entrypoint leaks target build setting %q into trusted host preparation", forbidden)
		}
	}
}

func TestPrepareCallerActionUsesScrubbedPlanEnvironment(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "actions", "prepare-caller", "action.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Runs struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	if len(action.Runs.Steps) != 1 {
		t.Fatalf("prepare-caller steps = %#v", action.Runs.Steps)
	}
	step := action.Runs.Steps[0]
	if step.Shell != "bash" || step.WorkingDirectory != "${{ github.workspace }}" || !strings.Contains(step.Run, "prepare-caller.sh") {
		t.Fatalf("prepare-caller step = %#v", step)
	}
	wantEnvironment := map[string]string{
		"SLIVER_EXT_E2E_PREPARE_COMMAND": "${{ inputs.command }}",
		"SLIVER_EXT_E2E_PHASE":           "plan",
		"SLIVER_EXT_E2E_TARGET":          "",
		"SLIVER_EXT_E2E_OS":              "",
		"SLIVER_EXT_E2E_ARCH":            "",
	}
	for name, want := range wantEnvironment {
		if got := scalar(step.Env[name]); got != want {
			t.Fatalf("prepare-caller env %s = %q, want %q", name, got, want)
		}
	}
	if len(step.Env) != len(wantEnvironment) {
		t.Fatalf("prepare-caller env contains unexpected values: %#v", step.Env)
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "dist", "prepare-caller.sh"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	if !strings.Contains(contents, "exec env -i") {
		t.Fatal("prepare-caller script does not replace its environment")
	}
	for _, forbidden := range []string{"GITHUB_ENV=", "GITHUB_PATH=", "GITHUB_OUTPUT=", "GITHUB_STATE=", "GITHUB_TOKEN=", "ACTIONS_RUNTIME_TOKEN="} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("prepare-caller script forwards %s", forbidden)
		}
	}
}

func readWorkflow(t *testing.T) workflowFile {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", "e2e.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowFile
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&workflow); err != nil {
		t.Fatalf("decode workflow: %v", err)
	}
	return workflow
}

func readRepositoryFile(t *testing.T, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func stepIndex(t *testing.T, steps []workflowStep, name string) int {
	t.Helper()
	for index, step := range steps {
		if step.Name == name {
			return index
		}
	}
	t.Fatalf("step %q is missing", name)
	return -1
}

func countSteps(steps []workflowStep, name string) int {
	count := 0
	for _, step := range steps {
		if step.Name == name {
			count++
		}
	}
	return count
}

func scalar(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	case bool:
		if value {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(value)
	}
}
