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
