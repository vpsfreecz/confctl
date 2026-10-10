package extension

import "encoding/json"

// Registry is an explicitly supplied schema1 declaration bound to a finite set
// of configuration source files. It is independent of the RPC protocol version.
type Registry struct {
	Schema       int            `json:"schema"`
	BoundSources []BoundSource  `json:"bound_sources"`
	Extensions   []Registration `json:"extensions"`
}

type BoundSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Registration struct {
	ID       string    `json:"id"`
	Protocol Version   `json:"protocol"`
	Argv     []string  `json:"argv"`
	Groups   []Group   `json:"groups"`
	Commands []Command `json:"commands"`
	Hooks    []Hook    `json:"hooks"`
}

type Group struct {
	Path        []string `json:"path"`
	Description string   `json:"description"`
}

type Command struct {
	Path        []string   `json:"path"`
	Handler     string     `json:"handler"`
	Description string     `json:"description"`
	OptionSets  []string   `json:"option_sets"`
	Options     []Option   `json:"options"`
	Arguments   []Argument `json:"arguments"`
}

// Default is nil when absent and contains the literal JSON null when explicit.
// Integer defaults are decimal JSON integers; CLI radix syntax is separate.
type Option struct {
	Key         string          `json:"key"`
	Names       []string        `json:"names"`
	Kind        string          `json:"kind"`
	Default     json.RawMessage `json:"default,omitempty"`
	Multiple    bool            `json:"multiple"`
	Negatable   bool            `json:"negatable"`
	Choices     []string        `json:"choices"`
	Metavar     string          `json:"metavar"`
	Description string          `json:"description"`
}

type Argument struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic"`
}

type Hook struct {
	Event   string `json:"event"`
	Handler string `json:"handler"`
	Order   int    `json:"order"`
}
