package manifest

type rawConfig struct {
	Schema   string          `yaml:"schema"`
	Sliver   rawSliverConfig `yaml:"sliver"`
	Defaults rawSettings     `yaml:"defaults"`
	Suites   []rawSuite      `yaml:"suites"`
}

type rawSliverConfig struct {
	Version string `yaml:"version"`
}

type rawSuite struct {
	Name        string          `yaml:"name"`
	Target      rawTarget       `yaml:"target"`
	Tests       []rawTest       `yaml:"tests"`
	Collections []rawCollection `yaml:"collections"`
}

type rawTarget struct {
	OS   string `yaml:"os"`
	Arch string `yaml:"arch"`
}

type rawTest struct {
	Name        string `yaml:"name"`
	Library     string `yaml:"library"`
	rawSettings `yaml:",inline"`
}

type rawCollection struct {
	Name        string                 `yaml:"name"`
	Glob        string                 `yaml:"glob"`
	Overrides   map[string]rawSettings `yaml:"overrides"`
	rawSettings `yaml:",inline"`
}

type rawSettings struct {
	Export         *string         `yaml:"export"`
	Init           *string         `yaml:"init"`
	ArgsUTF8       *string         `yaml:"args_utf8"`
	ArgsBase64     *string         `yaml:"args_base64"`
	Modes          *[]string       `yaml:"modes"`
	Repeat         *int            `yaml:"repeat"`
	CommandTimeout *string         `yaml:"command_timeout"`
	Expect         *rawExpectation `yaml:"expect"`
}

type rawExpectation struct {
	SuccessOnly *bool                 `yaml:"success_only"`
	Output      *rawOutputExpectation `yaml:"output"`
}

type rawOutputExpectation struct {
	Equals       *string  `yaml:"equals"`
	Contains     []string `yaml:"contains"`
	NotContains  []string `yaml:"not_contains"`
	Matches      []string `yaml:"matches"`
	SHA256       string   `yaml:"sha256"`
	EqualsBase64 *string  `yaml:"equals_base64"`
}
