package harness

import (
	"context"
	"time"

	"github.com/sliverarmory/sliver-test-ext/internal/results"
)

type OutputExpectation struct {
	Equals          *string
	Contains        []string
	NotContains     []string
	Matches         []string
	SHA256          string
	EqualsBase64    string
	HasEqualsBase64 bool
}

type Expectation struct {
	SuccessOnly bool
	Output      OutputExpectation
}

// Step is one call against an already registered library on a live implant.
// A case's entire ordered step list is repeated on that same implant.
type Step struct {
	Name            string
	Export          string
	Arguments       []byte
	InterpolatePort bool
	CommandTimeout  time.Duration
	Expectation     Expectation
	TCPProbe        *TCPProbe
	CommandProbe    *CommandProbe
}

type TCPProbe struct {
	Port     uint16
	AutoPort bool
	State    string
	Timeout  time.Duration
	Banner   []byte
}

// CommandProbe runs a caller-owned executable after a successful extension
// call and optional TCP probe. Path is resolved and confined by the manifest.
type CommandProbe struct {
	Path    string
	Args    []string
	Timeout time.Duration
}

type Case struct {
	ID                string
	Name              string
	Suite             string
	Collection        string
	Target            Target
	LibraryPath       string
	LibraryReportPath string
	LibraryData       []byte
	LibrarySHA256     string
	Export            string
	Init              string
	Arguments         []byte
	Modes             []string
	Repeat            int
	CommandTimeout    time.Duration
	Expectation       Expectation
	Steps             []Step
}

type Target struct {
	OS   string
	Arch string
}

func (target Target) String() string { return target.OS + "/" + target.Arch }

type Options struct {
	Workspace      string
	ServerPath     string
	Target         Target
	Cases          []Case
	ResultsDir     string
	StateFile      string
	Sliver         results.Provenance
	ActionVersion  string
	OverallTimeout time.Duration
	StartupTimeout time.Duration
	ConnectTimeout time.Duration
	BeaconInterval time.Duration
	Logf           func(string, ...any)
}

type Runner interface {
	Run(context.Context, Options) (*results.Report, error)
}
