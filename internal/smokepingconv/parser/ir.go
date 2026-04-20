package parser

// SPRoot is the root of the parsed IR. Ordered slices are used instead of maps
// everywhere ordering matters for deterministic output.
type SPRoot struct {
	Database map[string]string `json:"database,omitempty"`
	Probes   []SPProbe         `json:"probes,omitempty"`
	Alerts   []SPAlert         `json:"alerts,omitempty"`
	Targets  *SPNode           `json:"targets,omitempty"`
	Unknown  []SPUnknown       `json:"unknown,omitempty"`
}

type SPProbe struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"` // inherited from parent for subprobes
	Params    map[string]string `json:"params,omitempty"`
	Keys      []string          `json:"keys,omitempty"` // param insertion order
	Subprobes []SPProbe         `json:"subprobes,omitempty"`
	File      string            `json:"file,omitempty"`
	LineNo    int               `json:"line_no,omitempty"`
}

type SPAlert struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params,omitempty"`
	Keys   []string          `json:"keys,omitempty"`
	File   string            `json:"file,omitempty"`
	LineNo int               `json:"line_no,omitempty"`
}

type SPNode struct {
	Name     string            `json:"name,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Keys     []string          `json:"keys,omitempty"`
	Children []*SPNode         `json:"children,omitempty"`
	File     string            `json:"file,omitempty"`
	LineNo   int               `json:"line_no,omitempty"`
}

type SPUnknown struct {
	Section string   `json:"section"`
	Lines   []string `json:"lines,omitempty"`
}
