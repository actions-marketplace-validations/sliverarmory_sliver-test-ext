// Package manifest loads and validates sliver-ext-e2e/v1 manifests.
package manifest

import "time"

const (
	SchemaV1    = "sliver-ext-e2e/v1"
	ModeSession = "session"
	ModeBeacon  = "beacon"
)

// Manifest contains normalized configuration and one plan per concrete library.
// Plan.Library resolves inside Workspace for direct use, or inside a private
// library root for the reusable workflow. Plan.LibraryPath is portable and
// relative to that root.
type Manifest struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
	Config    Config `json:"config"`
	Plans     []Plan `json:"plans"`
}

type Config struct {
	Schema   string       `json:"schema"`
	Sliver   SliverConfig `json:"sliver"`
	Defaults Settings     `json:"defaults"`
	Suites   []Suite      `json:"suites"`
}

type SliverConfig struct {
	Version string `json:"version"`
}

type Target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

func (t Target) Key() string { return t.OS + "/" + t.Arch }

type Suite struct {
	Name        string       `json:"name"`
	Target      Target       `json:"target"`
	Tests       []Test       `json:"tests,omitempty"`
	Collections []Collection `json:"collections,omitempty"`
}

type Test struct {
	Name     string   `json:"name"`
	Library  string   `json:"library"`
	Settings Settings `json:"settings"`
}

// Collection records the normalized shared settings and matched libraries.
type Collection struct {
	Name      string   `json:"name"`
	Glob      string   `json:"glob"`
	Settings  Settings `json:"settings"`
	Libraries []string `json:"libraries"`
}

type Settings struct {
	Export         string        `json:"export"`
	Init           string        `json:"init,omitempty"`
	Arguments      []byte        `json:"arguments,omitempty"`
	Modes          []string      `json:"modes"`
	Repeat         int           `json:"repeat"`
	CommandTimeout time.Duration `json:"command_timeout"`
	Expect         Expectation   `json:"expect"`
}

type Expectation struct {
	SuccessOnly bool              `json:"success_only"`
	Output      OutputExpectation `json:"output"`
}

type OutputExpectation struct {
	Equals       *string  `json:"equals,omitempty"`
	Contains     []string `json:"contains,omitempty"`
	NotContains  []string `json:"not_contains,omitempty"`
	Matches      []string `json:"matches,omitempty"`
	SHA256       string   `json:"sha256,omitempty"`
	EqualsBytes  []byte   `json:"equals_bytes,omitempty"`
	HasEqualsB64 bool     `json:"-"`
}

// Plan describes one library registration and export invocation. Modes and
// Repeat specify how many independent implant executions the harness performs.
type Plan struct {
	ID          string   `json:"id"`
	Suite       string   `json:"suite"`
	Name        string   `json:"name"`
	Collection  string   `json:"collection,omitempty"`
	Library     string   `json:"library"`
	LibraryPath string   `json:"library_path"`
	Target      Target   `json:"target"`
	Settings    Settings `json:"settings"`
}

type Matrix struct {
	Include []MatrixEntry `json:"include"`
}

type MatrixEntry struct {
	Target string `json:"target"`
	Runner string `json:"runner"`
	Key    string `json:"key"`
}
