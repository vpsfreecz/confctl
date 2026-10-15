// Package harness describes process-boundary cases independently of either CLI.
package harness

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
)

const OracleRevision = "cc40679d267165aecfa569128438bb55fe910268"
const SiteRevision = "ae670dc0d4a5d43adf9560da1a6a0a35925cd2e5"

type File struct {
	Text string `json:"text,omitempty"`
	Link string `json:"link,omitempty"`
	Mode uint32 `json:"mode,omitempty"`
}
type Rule struct {
	Tool       string          `json:"tool"`
	Host       string          `json:"host,omitempty"`
	Argv       []string        `json:"argv,omitempty"`
	Contains   string          `json:"contains,omitempty"`
	ReadStdin  bool            `json:"read_stdin,omitempty"`
	Stdin      *string         `json:"stdin,omitempty"`
	Occurrence int             `json:"occurrence,omitempty"`
	Stdout     string          `json:"stdout,omitempty"`
	Stderr     string          `json:"stderr,omitempty"`
	Exit       int             `json:"exit,omitempty"`
	DelayMS    int             `json:"delay_ms,omitempty"`
	Signal     string          `json:"signal,omitempty"`
	Writes     map[string]File `json:"writes,omitempty"`
}
type Source struct {
	Revision string `json:"revision"`
	Path     string `json:"path"`
	Lines    string `json:"lines"`
}
type Case struct {
	FixtureDigest   string            `json:"-"`
	ExpectedNoCalls bool              `json:"expected_no_calls"`
	Schema          int               `json:"schema"`
	ID              string            `json:"id"`
	Tier            string            `json:"tier"`
	Sources         []Source          `json:"sources"`
	Argv            []string          `json:"argv"`
	Env             map[string]string `json:"env"`
	Stdin           string            `json:"stdin"`
	TTY             bool              `json:"tty"`
	DeadlineMS      int               `json:"deadline_ms"`
	Causal          []Constraint      `json:"causal"`
	Files           map[string]File   `json:"files"`
	Rules           []Rule            `json:"rules"`
	Expected        string            `json:"expected"`
	Extensions      bool              `json:"extensions,omitempty"`
	Hook            string            `json:"hook,omitempty"`
	Selected        []string          `json:"selected,omitempty"`
	Normalize       Normalization     `json:"normalize"`
}
type Event struct {
	Occurrence int               `json:"occurrence"`
	Stdout     string            `json:"stdout"`
	Stderr     string            `json:"stderr"`
	StdinMode  string            `json:"stdin_mode"`
	Tool       string            `json:"tool"`
	Host       string            `json:"host,omitempty"`
	Argv       []string          `json:"argv"`
	Env        map[string]string `json:"env"`
	Stdin      string            `json:"stdin"`
	CWD        string            `json:"cwd"`
	PID        int               `json:"pid"`
	Start      int64             `json:"start_ns"`
	End        int64             `json:"end_ns"`
	Exit       int               `json:"exit"`
	Signal     string            `json:"signal,omitempty"`
	Rule       int               `json:"rule"`
	Writes     map[string]File   `json:"writes,omitempty"`
}
type Observation struct {
	ConfigRoot       string            `json:"config_root"`
	RunRoot          string            `json:"run_root"`
	Schema           int               `json:"schema"`
	Case             string            `json:"case"`
	Executable       string            `json:"executable"`
	ExecutableSHA256 string            `json:"executable_sha256"`
	ContractRevision string            `json:"contract_revision"`
	FixtureSHA256    string            `json:"fixture_sha256"`
	SourceRevision   string            `json:"source_revision"`
	Stdout           string            `json:"stdout"`
	Stderr           string            `json:"stderr"`
	Exit             int               `json:"exit"`
	Signal           string            `json:"signal,omitempty"`
	Before           map[string]File   `json:"before"`
	After            map[string]File   `json:"after"`
	Events           []Event           `json:"events"`
	GitBefore        map[string]string `json:"git_before"`
	GitAfter         map[string]string `json:"git_after"`
	WallNS           int64             `json:"wall_ns"`
	UserNS           int64             `json:"user_ns"`
	SystemNS         int64             `json:"system_ns"`
	MaxRSSKB         int64             `json:"max_rss_kb"`
}

func Load(path string) (Case, error) {
	var c Case
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	c.FixtureDigest = fmt.Sprintf("%x", sha256.Sum256(b))
	if c.Schema != 1 || c.ID == "" || c.Tier == "" || len(c.Sources) == 0 {
		return c, fmt.Errorf("invalid case identity/provenance: %s", path)
	}
	if c.DeadlineMS <= 0 {
		c.DeadlineMS = 30000
	}
	for _, s := range c.Sources {
		if s.Revision != OracleRevision && s.Revision != SiteRevision {
			return c, fmt.Errorf("unpinned source %s", s.Revision)
		}
	}
	return c, nil
}

type Selector struct {
	Tool          string `json:"tool"`
	Host          string `json:"host,omitempty"`
	Contains      string `json:"contains,omitempty"`
	StdinContains string `json:"stdin_contains,omitempty"`
	Occurrence    int    `json:"occurrence,omitempty"`
}
type Constraint struct {
	Before Selector `json:"before"`
	After  Selector `json:"after"`
}
