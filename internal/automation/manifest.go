// Package automation implements repository-local, trusted TypeScript automation.
package automation

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is the Tandem integration metadata in a script's leading @tandem
// comment. It intentionally describes only Tandem-provided capabilities; the
// script itself executes as ordinary trusted host code.
type Manifest struct {
	Name        string           `yaml:"name" json:"name"`
	Description string           `yaml:"description,omitempty" json:"description,omitempty"`
	Tools       []ToolPermission `yaml:"tools,omitempty" json:"tools,omitempty"`
	Browser     *BrowserSpec     `yaml:"browser,omitempty" json:"browser,omitempty"`
	Schedule    *ScheduleSpec    `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Wake        *WakeSpec        `yaml:"wake,omitempty" json:"wake,omitempty"`
}

type ToolPermission struct {
	Name   string `yaml:"name" json:"name"`
	Reason string `yaml:"reason" json:"reason"`
}

type BrowserSpec struct {
	Snapshot string `yaml:"snapshot" json:"snapshot"`
}

type ScheduleSpec struct {
	Cron        string `yaml:"cron" json:"cron"`
	Timezone    string `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Concurrency string `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
}

type WakeSpec struct {
	AgentProfile string `yaml:"agentProfile" json:"agentProfile"`
	Prompt       string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
}

var ErrNoFrontmatter = errors.New("script has no leading @tandem frontmatter")

// ParseManifest parses YAML from a leading documentation comment whose first
// meaningful line is @tandem. Unknown YAML fields are rejected to catch grant
// and scheduling typos before a script is registered.
func ParseManifest(source []byte) (Manifest, error) {
	s := strings.TrimPrefix(string(source), "\ufeff")
	s = strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(s, "/**") {
		return Manifest{}, ErrNoFrontmatter
	}
	end := strings.Index(s[3:], "*/")
	if end < 0 {
		return Manifest{}, fmt.Errorf("unterminated leading documentation comment")
	}
	body := s[3 : 3+end]
	lines := strings.Split(body, "\n")
	for i := range lines {
		line := strings.TrimSuffix(lines[i], "\r")
		left := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(left, "*") {
			line = strings.TrimPrefix(left, "*")
			line = strings.TrimPrefix(line, " ")
		} else {
			// The opening /** consumes the indentation column that would
			// otherwise precede each undecorated frontmatter line.
			line = strings.TrimPrefix(line, " ")
		}
		lines[i] = strings.TrimRight(line, " \t")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 || lines[0] != "@tandem" {
		return Manifest{}, ErrNoFrontmatter
	}
	yamlText := strings.Join(lines[1:], "\n")
	var out Manifest
	dec := yaml.NewDecoder(bytes.NewBufferString(yamlText))
	dec.KnownFields(true)
	if err := dec.Decode(&out); err != nil {
		return Manifest{}, fmt.Errorf("parse @tandem frontmatter: %w", err)
	}
	if err := out.Validate(); err != nil {
		return Manifest{}, err
	}
	return out, nil
}

func (m Manifest) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("@tandem name is required")
	}
	seen := make(map[string]struct{}, len(m.Tools))
	for i, tool := range m.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("@tandem tools[%d].name is required", i)
		}
		if strings.TrimSpace(tool.Reason) == "" {
			return fmt.Errorf("@tandem tools[%d].reason is required", i)
		}
		if _, ok := seen[tool.Name]; ok {
			return fmt.Errorf("@tandem tool %q is listed more than once", tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}
	if m.Browser != nil && strings.TrimSpace(m.Browser.Snapshot) == "" {
		return errors.New("@tandem browser.snapshot is required when browser is present")
	}
	if m.Schedule != nil {
		if strings.TrimSpace(m.Schedule.Cron) == "" {
			return errors.New("@tandem schedule.cron is required when schedule is present")
		}
		switch m.Schedule.Concurrency {
		case "", "skip", "queue":
		default:
			return fmt.Errorf("@tandem schedule.concurrency must be skip or queue, got %q", m.Schedule.Concurrency)
		}
	}
	if m.Wake != nil && strings.TrimSpace(m.Wake.AgentProfile) == "" {
		return errors.New("@tandem wake.agentProfile is required when wake is present")
	}
	return nil
}
