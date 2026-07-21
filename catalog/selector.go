//go:build windows

package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// Operator is a selector requirement's comparison operation.
type Operator string

const (
	OpEquals    Operator = "="
	OpNotEquals Operator = "!="
	OpExists    Operator = "exists"
	OpNotExists Operator = "not-exists"
)

// Requirement is one comma-separated selector term. Supported forms are
// key=value, key!=value, key, and !key.
type Requirement struct {
	Key      string   `json:"key"`
	Operator Operator `json:"operator"`
	Value    string   `json:"value,omitempty"`
}

// Selector is an AND of all requirements. The empty selector matches every
// catalog entry.
type Selector struct {
	Requirements []Requirement `json:"requirements,omitempty"`
}

func ParseSelector(raw string) (Selector, error) {
	if strings.TrimSpace(raw) == "" {
		return Selector{}, nil
	}

	parts := strings.Split(raw, ",")
	requirements := make([]Requirement, 0, len(parts))
	for _, part := range parts {
		term := strings.TrimSpace(part)
		if term == "" {
			return Selector{}, fmt.Errorf("invalid empty selector requirement")
		}

		var requirement Requirement
		switch {
		case strings.HasPrefix(term, "!") && strings.Contains(term[1:], "="):
			return Selector{}, fmt.Errorf("invalid selector requirement %q", term)
		case strings.HasPrefix(term, "!"):
			requirement = Requirement{Key: term[1:], Operator: OpNotExists}
		case strings.Contains(term, "!="):
			fields := strings.SplitN(term, "!=", 2)
			requirement = Requirement{Key: strings.TrimSpace(fields[0]), Operator: OpNotEquals, Value: strings.TrimSpace(fields[1])}
		case strings.Contains(term, "="):
			fields := strings.SplitN(term, "=", 2)
			requirement = Requirement{Key: strings.TrimSpace(fields[0]), Operator: OpEquals, Value: strings.TrimSpace(fields[1])}
		default:
			requirement = Requirement{Key: term, Operator: OpExists}
		}

		if !validLabelKey.MatchString(requirement.Key) {
			return Selector{}, fmt.Errorf("invalid selector label key %q", requirement.Key)
		}
		if requirement.Operator == OpEquals || requirement.Operator == OpNotEquals {
			if err := ValidateLabels(map[string]string{requirement.Key: requirement.Value}); err != nil {
				return Selector{}, fmt.Errorf("invalid selector: %w", err)
			}
		}
		requirements = append(requirements, requirement)
	}

	return Selector{Requirements: requirements}, nil
}

func (s Selector) Matches(labels map[string]string) bool {
	for _, requirement := range s.Requirements {
		value, exists := labels[requirement.Key]
		switch requirement.Operator {
		case OpEquals:
			if !exists || value != requirement.Value {
				return false
			}
		case OpNotEquals:
			if exists && value == requirement.Value {
				return false
			}
		case OpExists:
			if !exists {
				return false
			}
		case OpNotExists:
			if exists {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// String returns a canonical selector. Requirements are sorted so saved views
// and JSON diffs do not depend on input order.
func (s Selector) String() string {
	terms := make([]string, 0, len(s.Requirements))
	for _, requirement := range s.Requirements {
		switch requirement.Operator {
		case OpEquals:
			terms = append(terms, requirement.Key+"="+requirement.Value)
		case OpNotEquals:
			terms = append(terms, requirement.Key+"!="+requirement.Value)
		case OpExists:
			terms = append(terms, requirement.Key)
		case OpNotExists:
			terms = append(terms, "!"+requirement.Key)
		}
	}
	sort.Strings(terms)
	return strings.Join(terms, ",")
}
