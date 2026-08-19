package config

// RequestScopedErrorRule configures custom handling for matching upstream errors.
type RequestScopedErrorRule struct {
	Status      int      `yaml:"status,omitempty" json:"status,omitempty"`
	Match       []string `yaml:"match,omitempty" json:"match,omitempty"`
	MatchRegexr []string `yaml:"match-regexr,omitempty" json:"match-regexr,omitempty"`
	Action      string   `yaml:"action,omitempty" json:"action,omitempty"`
}
