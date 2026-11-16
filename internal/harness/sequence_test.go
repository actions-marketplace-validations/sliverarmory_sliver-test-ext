package harness

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sliverarmory/sliver-test-ext/internal/results"
)

func TestTCPProbeOpenBannerAndClosed(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			_, err = connection.Write([]byte("RFB 003.008\n"))
			_ = connection.Close()
		}
		done <- err
	}()
	probe := TCPProbe{AutoPort: true, State: "open", Banner: []byte("RFB 003.008\n"), Timeout: time.Second}
	if err := probeTCP(context.Background(), probe, port); err != nil {
		t.Fatalf("open banner probe: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probeTCP(context.Background(), TCPProbe{Port: port, State: "closed", Timeout: time.Second}, 0); err != nil {
		t.Fatalf("closed probe: %v", err)
	}
}

func TestTCPProbeRejectsWrongBannerAndTimesOut(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	done := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_, _ = connection.Write([]byte("RFB 003.003\n"))
			_ = connection.Close()
		}
		close(done)
	}()
	err = probeTCP(context.Background(), TCPProbe{Port: port, State: "open", Banner: []byte("RFB 003.008\n"), Timeout: time.Second}, 0)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong banner error = %v", err)
	}
	<-done
	_ = listener.Close()
	err = probeTCP(context.Background(), TCPProbe{Port: port, State: "open", Timeout: 50 * time.Millisecond}, 0)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("closed port open-probe error = %v", err)
	}
	if err := probeTCP(context.Background(), TCPProbe{State: "closed", Timeout: time.Second, Banner: []byte("x")}, port); err == nil {
		t.Fatal("closed probe accepted a banner")
	}
}

func TestSequenceSkipsRemainingStepsAndRepeats(t *testing.T) {
	caseSpec := Case{ID: "suite/test", Suite: "suite", Name: "test", Repeat: 2, Steps: []Step{
		{Name: "start", Export: "Run"}, {Name: "status", Export: "Run"}, {Name: "stop", Export: "Run"},
	}}
	runner := &runtimeHarness{report: results.New("linux/amd64")}
	runner.skipRemainingSteps(caseSpec, "session", 1, 0)
	if len(runner.report.Cases) != 5 {
		t.Fatalf("skipped results = %d, want 5", len(runner.report.Cases))
	}
	for _, result := range runner.report.Cases {
		if result.Status != "skipped" || result.Stage != "prior-step-failed" || result.Step == "" || result.StepIndex < 1 {
			t.Fatalf("invalid skipped result: %+v", result)
		}
	}
	if runner.report.Cases[0].Step != "status" || runner.report.Cases[0].Repeat != 1 || runner.report.Cases[4].Step != "stop" || runner.report.Cases[4].Repeat != 2 {
		t.Fatalf("skip order = %+v", runner.report.Cases)
	}
	if !sequenceNeedsPort([]Step{{CommandProbe: &CommandProbe{Args: []string{"--port", "${TCP_PORT}"}}}}) {
		t.Fatal("command probe placeholder did not request an automatic port")
	}
}

func TestSequenceUsesStepExportsWithoutLegacyCaseExport(t *testing.T) {
	server := filepath.Join(t.TempDir(), "sliver-server")
	if err := os.WriteFile(server, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := Target{OS: "linux", Arch: "amd64"}
	opts := Options{Workspace: t.TempDir(), ServerPath: server, ResultsDir: t.TempDir(), Target: target, Cases: []Case{{
		Name: "sequence", Target: target, Repeat: 2, Steps: []Step{{Name: "start", Export: "Run"}, {Name: "stop", Export: "Stop"}},
	}}}
	if err := validateRunOptions(opts); err != nil {
		t.Fatalf("sequence with step exports: %v", err)
	}
	opts.Cases[0].Steps[1].Export = ""
	if err := validateRunOptions(opts); err == nil || !strings.Contains(err.Error(), "requires a name and export") {
		t.Fatalf("missing step export error = %v", err)
	}
}

func TestSequenceRunsOrderedStepsAndRepeatsOnOneProcess(t *testing.T) {
	process := &managedProcess{done: make(chan struct{})}
	runner := &runtimeHarness{ctx: context.Background(), opts: Options{ResultsDir: t.TempDir()}, report: results.New("linux/amd64")}
	testCase := Case{ID: "suite/sequence", Suite: "suite", Name: "sequence", Repeat: 2, Steps: []Step{
		{Name: "start", Export: "Run", Arguments: []byte("start --port ${TCP_PORT}"), InterpolatePort: true, Expectation: Expectation{SuccessOnly: true}},
		{Name: "status", Export: "Run", Arguments: []byte("status"), Expectation: Expectation{SuccessOnly: true}},
		{Name: "stop", Export: "Stop", Arguments: []byte("stop"), Expectation: Expectation{SuccessOnly: true}},
	}}
	var calls []string
	var selectedPort string
	runner.runSequenceWithCall(testCase, connectedTarget{}, process, "session", "", "", "", func(_ connectedTarget, current *managedProcess, call Case) ([]byte, error) {
		if current != process {
			t.Fatal("sequence changed implant process")
		}
		calls = append(calls, call.Export+":"+string(call.Arguments))
		if strings.HasPrefix(string(call.Arguments), "start --port ") {
			port := strings.TrimPrefix(string(call.Arguments), "start --port ")
			if port == "" || strings.Contains(port, "${") {
				t.Fatalf("automatic port was not substituted: %q", port)
			}
			if selectedPort == "" {
				selectedPort = port
			} else if selectedPort != port {
				t.Fatalf("repeat selected a different port: %s, %s", selectedPort, port)
			}
		}
		return []byte("ok"), nil
	})
	if len(calls) != 6 || len(runner.report.Cases) != 6 {
		t.Fatalf("calls=%v results=%+v", calls, runner.report.Cases)
	}
	for index, result := range runner.report.Cases {
		if result.Status != "pass" || result.StepIndex != index%3+1 || result.Repeat != index/3+1 || result.OutputFile == "" {
			t.Fatalf("result %d = %+v", index, result)
		}
		if !strings.Contains(result.OutputFile, result.Step) {
			t.Fatalf("step output path does not include name: %+v", result)
		}
	}
}

func TestSequenceFailureAndProbeFailureSkipRemainingSteps(t *testing.T) {
	process := &managedProcess{done: make(chan struct{})}
	want := "good"
	steps := []Step{
		{Name: "start", Export: "Run", Expectation: Expectation{Output: OutputExpectation{Equals: &want}}},
		{Name: "status", Export: "Run", Expectation: Expectation{SuccessOnly: true}},
	}
	runner := &runtimeHarness{ctx: context.Background(), opts: Options{ResultsDir: t.TempDir()}, report: results.New("linux/amd64")}
	caseSpec := Case{ID: "suite/sequence", Repeat: 2, Steps: steps}
	runner.runSequenceWithCall(caseSpec, connectedTarget{}, process, "session", "", "", "", func(connectedTarget, *managedProcess, Case) ([]byte, error) {
		return []byte("bad"), nil
	})
	if len(runner.report.Cases) != 4 || runner.report.Cases[0].Status != "fail" || runner.report.Cases[0].Stage != "assert-output" {
		t.Fatalf("assertion failure results = %+v", runner.report.Cases)
	}
	for _, result := range runner.report.Cases[1:] {
		if result.Status != "skipped" || result.Stage != "prior-step-failed" {
			t.Fatalf("unskipped following step = %+v", result)
		}
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	steps[0].Expectation = Expectation{SuccessOnly: true}
	steps[0].TCPProbe = &TCPProbe{Port: port, State: "closed", Timeout: 50 * time.Millisecond}
	runner.report = results.New("linux/amd64")
	runner.runSequenceWithCall(caseSpec, connectedTarget{}, process, "session", "", "", "", func(connectedTarget, *managedProcess, Case) ([]byte, error) {
		return []byte("good"), nil
	})
	if len(runner.report.Cases) != 4 || runner.report.Cases[0].Stage != "tcp-probe" {
		t.Fatalf("TCP probe failure results = %+v", runner.report.Cases)
	}
}

func TestCommandProbeUsesCredentialFreeEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	workspace := t.TempDir()
	resultsDir := t.TempDir()
	probePath := filepath.Join(workspace, "probe.sh")
	probeScript := "#!/bin/sh\nprintf 'port=%s\\n' \"$1\"\nenv | sort\n"
	if err := os.WriteFile(probePath, []byte(probeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cloud-secret")
	t.Setenv("DISPLAY", ":99")
	t.Setenv("XAUTHORITY", "auth/xauthority")
	runner := &runtimeHarness{
		ctx: context.Background(), opts: Options{Workspace: workspace, ResultsDir: resultsDir}, tracker: newStateTracker(""),
	}
	relative, err := runner.runCommandProbe(Case{ID: "suite/test"}, "session", 1, 1,
		CommandProbe{Path: probePath, Args: []string{"${TCP_PORT}"}, Timeout: time.Second}, 5901,
		filepath.Join(workspace, "home"), filepath.Join(workspace, "tmp"))
	if err != nil {
		t.Fatalf("command probe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(resultsDir, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	for _, want := range []string{"port=5901", "DISPLAY=:99", "XAUTHORITY=" + filepath.Join(workspace, "auth/xauthority")} {
		if !strings.Contains(output, want) {
			t.Errorf("probe output missing %q: %s", want, output)
		}
	}
	for _, forbidden := range []string{"GITHUB_TOKEN=", "AWS_SECRET_ACCESS_KEY="} {
		if strings.Contains(output, forbidden) {
			t.Errorf("probe output leaked %q", forbidden)
		}
	}
	if err := os.WriteFile(probePath, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = runner.runCommandProbe(Case{ID: "suite/test"}, "session", 1, 2,
		CommandProbe{Path: probePath, Timeout: 50 * time.Millisecond}, 0,
		filepath.Join(workspace, "home"), filepath.Join(workspace, "tmp"))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("command probe timeout error = %v", err)
	}
}

func TestBoundedOutputWriterCapsProbeLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := &boundedOutputWriter{file: file, remaining: 5}
	for _, part := range []string{"abc", "defghi"} {
		if written, err := fmt.Fprint(writer, part); err != nil || written != len(part) {
			t.Fatalf("write %q = %d, %v", part, written, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abcde" {
		t.Fatalf("bounded output = %q, want abcde", data)
	}
}
