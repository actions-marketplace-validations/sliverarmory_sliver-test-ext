package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sliverarmory/sliver-test-ext/internal/results"
)

const (
	defaultProbeTimeout = 10 * time.Second
	maxProbeTimeout     = 2 * time.Minute
	maxProbeBanner      = 4096
	probePollInterval   = 100 * time.Millisecond
	probeDialTimeout    = 500 * time.Millisecond
)

var tcpPortPlaceholder = []byte("${TCP_PORT}")

func (runner *runtimeHarness) runSequence(testCase Case, target connectedTarget, process *managedProcess, mode, home, temp, implantLog string) {
	runner.runSequenceWithCall(testCase, target, process, mode, home, temp, implantLog, runner.execute)
}

type extensionCall func(connectedTarget, *managedProcess, Case) ([]byte, error)

func (runner *runtimeHarness) runSequenceWithCall(testCase Case, target connectedTarget, process *managedProcess, mode, home, temp, implantLog string, callExtension extensionCall) {
	port := uint16(0)
	if sequenceNeedsPort(testCase.Steps) {
		var err error
		port, err = unusedLoopbackPort()
		if err != nil {
			runner.addStepFailure(testCase, testCase.Steps[0], 1, mode, 1, "allocate-port", err, implantLog)
			runner.skipRemainingSteps(testCase, mode, 1, 0)
			return
		}
	}
	for repeat := 1; repeat <= testCase.Repeat; repeat++ {
		for index, step := range testCase.Steps {
			if err := runner.ctx.Err(); err != nil {
				runner.addStepFailure(testCase, step, index+1, mode, repeat, "run-context", err, implantLog)
				runner.skipRemainingSteps(testCase, mode, repeat, index)
				return
			}
			if !process.running() {
				runner.addStepFailure(testCase, step, index+1, mode, repeat, "implant-exited-before-step", process.failure("implant exited before the next step"), implantLog)
				runner.skipRemainingSteps(testCase, mode, repeat, index)
				return
			}
			call := testCase
			call.Export = step.Export
			call.Arguments = append([]byte(nil), step.Arguments...)
			call.CommandTimeout = step.CommandTimeout
			if call.CommandTimeout <= 0 {
				call.CommandTimeout = testCase.CommandTimeout
			}
			if step.InterpolatePort {
				call.Arguments = bytes.ReplaceAll(call.Arguments, tcpPortPlaceholder, []byte(strconv.Itoa(int(port))))
			}
			started := time.Now()
			output, callErr := callExtension(target, process, call)
			outputFile, outputErr := runner.writeStepOutput(testCase, mode, repeat, index+1, step.Name, output)
			stage := ""
			err := callErr
			if err != nil {
				stage = "call-extension"
			}
			if outputErr != nil {
				err = errors.Join(err, fmt.Errorf("persist extension output: %w", outputErr))
				if stage == "" {
					stage = "persist-output"
				}
			}
			if err == nil {
				if assertErr := AssertOutput(output, step.Expectation); assertErr != nil {
					err, stage = assertErr, "assert-output"
				}
			}
			if err == nil && step.TCPProbe != nil {
				if probeErr := probeTCP(runner.ctx, *step.TCPProbe, port); probeErr != nil {
					err, stage = probeErr, "tcp-probe"
				}
			}
			probeOutputFile := ""
			if err == nil && step.CommandProbe != nil {
				probeOutputFile, err = runner.runCommandProbe(testCase, mode, repeat, index+1, *step.CommandProbe, port, home, temp)
				if err != nil {
					stage = "command-probe"
				}
			}
			result := runner.stepResult(testCase, step, index+1, mode, repeat)
			result.Duration = time.Since(started)
			result.OutputFile = outputFile
			result.ProbeOutputFile = probeOutputFile
			if err != nil {
				result.Status = "fail"
				result.Stage = stage
				result.Error = runner.diagnostics(err, implantLog)
			}
			runner.report.Add(result)
			if err != nil {
				runner.skipRemainingSteps(testCase, mode, repeat, index)
				return
			}
		}
	}
}

func sequenceNeedsPort(steps []Step) bool {
	for _, step := range steps {
		if step.InterpolatePort || step.TCPProbe != nil && step.TCPProbe.AutoPort {
			return true
		}
		if step.CommandProbe != nil {
			for _, argument := range step.CommandProbe.Args {
				if strings.Contains(argument, string(tcpPortPlaceholder)) {
					return true
				}
			}
		}
	}
	return false
}

func unusedLoopbackPort() (uint16, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve loopback port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("release loopback port: %w", err)
	}
	return uint16(port), nil
}

func (runner *runtimeHarness) stepResult(testCase Case, step Step, index int, mode string, repeat int) results.CaseResult {
	return results.CaseResult{
		ID: testCase.ID, Suite: testCase.Suite, Test: testCase.Name, Collection: testCase.Collection,
		Library: reportLibraryPath(testCase), LibrarySHA256: testCase.LibrarySHA256,
		Export: step.Export, Init: testCase.Init, Mode: mode, Repeat: repeat,
		Step: step.Name, StepIndex: index, Status: "pass",
	}
}

func (runner *runtimeHarness) addStepFailure(testCase Case, step Step, index int, mode string, repeat int, stage string, err error, implantLog string) {
	result := runner.stepResult(testCase, step, index, mode, repeat)
	result.Status, result.Stage, result.Error = "fail", stage, runner.diagnostics(err, implantLog)
	runner.report.Add(result)
}

func (runner *runtimeHarness) skipRemainingSteps(testCase Case, mode string, repeat, failedIndex int) {
	for nextRepeat := repeat; nextRepeat <= testCase.Repeat; nextRepeat++ {
		start := 0
		if nextRepeat == repeat {
			start = failedIndex + 1
		}
		for index := start; index < len(testCase.Steps); index++ {
			result := runner.stepResult(testCase, testCase.Steps[index], index+1, mode, nextRepeat)
			result.Status = "skipped"
			result.Stage = "prior-step-failed"
			runner.report.Add(result)
		}
	}
}

func (runner *runtimeHarness) writeStepOutput(testCase Case, mode string, repeat, index int, name string, output []byte) (string, error) {
	readable := safeName(name)
	if len(readable) > 48 {
		readable = readable[:48]
	}
	relative := filepath.Join("outputs", caseArtifactKey(testCase), fmt.Sprintf("%s-%03d-step-%03d-%s.bin", mode, repeat, index, readable))
	path := filepath.Join(runner.opts.ResultsDir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, output, 0o600); err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

func probeTCP(parent context.Context, probe TCPProbe, autoPort uint16) error {
	if probe.State != "open" && probe.State != "closed" {
		return fmt.Errorf("unsupported TCP probe state %q", probe.State)
	}
	if len(probe.Banner) > maxProbeBanner || probe.State == "closed" && len(probe.Banner) != 0 {
		return errors.New("TCP probe banner is invalid for the requested state")
	}
	port := probe.Port
	if probe.AutoPort {
		if port != 0 {
			return errors.New("automatic TCP probe port cannot also set a fixed port")
		}
		port = autoPort
	}
	if port == 0 {
		return errors.New("TCP probe port is not set")
	}
	timeout := probe.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	if timeout > maxProbeTimeout {
		return fmt.Errorf("TCP probe timeout exceeds %s", maxProbeTimeout)
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	last := "no attempt completed"
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("TCP probe %s %s timed out after %s (%s): %w", address, probe.State, timeout, last, err)
		}
		attemptTimeout := min(probeDialTimeout, time.Until(deadline(ctx)))
		connection, err := (&net.Dialer{Timeout: attemptTimeout}).DialContext(ctx, "tcp4", address)
		if probe.State == "closed" {
			if isConnectionRefused(err) {
				return nil
			}
			if err != nil && ctx.Err() == nil {
				return fmt.Errorf("TCP probe %s: %w", address, err)
			}
			if connection != nil {
				_ = connection.Close()
			}
			last = "listener still accepts connections"
		} else if err == nil {
			if len(probe.Banner) == 0 {
				_ = connection.Close()
				return nil
			}
			got := make([]byte, len(probe.Banner))
			_ = connection.SetDeadline(time.Now().Add(min(time.Second, time.Until(deadline(ctx)))))
			_, readErr := io.ReadFull(connection, got)
			_ = connection.Close()
			if readErr == nil {
				if bytes.Equal(got, probe.Banner) {
					return nil
				}
				return fmt.Errorf("TCP probe %s banner %q does not match %q", address, got, probe.Banner)
			}
			last = fmt.Sprintf("read banner: %v", readErr)
		} else if isConnectionRefused(err) || ctx.Err() != nil {
			last = fmt.Sprintf("dial: %v", err)
		} else {
			return fmt.Errorf("TCP probe %s: %w", address, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(probePollInterval):
		}
	}
}

func isConnectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// Windows connectex reports WSAECONNREFUSED (10061), which is distinct
	// from syscall.ECONNREFUSED on that platform.
	return runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10061))
}

func deadline(ctx context.Context) time.Time {
	value, _ := ctx.Deadline()
	return value
}

func (runner *runtimeHarness) runCommandProbe(testCase Case, mode string, repeat, index int, probe CommandProbe, autoPort uint16, home, temp string) (string, error) {
	if !filepath.IsAbs(probe.Path) {
		return "", errors.New("command probe executable path must be absolute")
	}
	realPath, err := filepath.EvalSymlinks(probe.Path)
	if err != nil {
		return "", fmt.Errorf("resolve command probe executable: %w", err)
	}
	workspace, err := filepath.EvalSymlinks(runner.opts.Workspace)
	if err != nil {
		return "", fmt.Errorf("resolve caller workspace: %w", err)
	}
	relativePath, err := filepath.Rel(workspace, realPath)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", errors.New("command probe executable is outside the caller workspace")
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return "", fmt.Errorf("inspect command probe executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("command probe executable is not a regular file")
	}
	if probe.Timeout <= 0 {
		probe.Timeout = defaultProbeTimeout
	}
	if probe.Timeout > maxProbeTimeout {
		return "", fmt.Errorf("command probe timeout exceeds %s", maxProbeTimeout)
	}
	arguments := make([]string, len(probe.Args))
	for index, argument := range probe.Args {
		if strings.Contains(argument, string(tcpPortPlaceholder)) && autoPort == 0 {
			return "", errors.New("command probe references ${TCP_PORT} without an allocated port")
		}
		arguments[index] = strings.ReplaceAll(argument, string(tcpPortPlaceholder), strconv.Itoa(int(autoPort)))
	}
	relative := filepath.Join("outputs", caseArtifactKey(testCase), fmt.Sprintf("%s-%03d-step-%03d-probe.log", mode, repeat, index))
	path := filepath.Join(runner.opts.ResultsDir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	process, err := startProcessWithOutputLimit(realPath, arguments, runner.opts.Workspace,
		sanitizedImplantEnv(os.Environ(), home, temp, runner.opts.Workspace), path, probeOutputLimit)
	if err != nil {
		return "", fmt.Errorf("start command probe: %w", err)
	}
	if err := runner.tracker.addPID(process.PID()); err != nil {
		return filepath.ToSlash(relative), errors.Join(fmt.Errorf("track command probe: %w", err), process.stop())
	}
	ctx, cancel := context.WithTimeout(runner.ctx, probe.Timeout)
	defer cancel()
	select {
	case <-process.done:
		err = process.err
	case <-ctx.Done():
		err = fmt.Errorf("command probe timed out after %s: %w", probe.Timeout, ctx.Err())
	}
	stopErr := runner.stopTrackedProcess(process)
	if err != nil {
		err = fmt.Errorf("command probe failed: %w\n%s", err, readLogTail(path))
	}
	return filepath.ToSlash(relative), errors.Join(err, stopErr)
}
