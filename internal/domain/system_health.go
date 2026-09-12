package domain

import "time"

type HealthState string

const (
	HealthHealthy  HealthState = "healthy"
	HealthWarning  HealthState = "warning"
	HealthCritical HealthState = "critical"
	HealthUnknown  HealthState = "unknown"
	HealthDisabled HealthState = "disabled"
)

type HealthResource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type HealthEvidence struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type HealthCheck struct {
	ID          string           `json:"id"`
	Category    string           `json:"category"`
	State       HealthState      `json:"state"`
	Title       string           `json:"title"`
	Summary     string           `json:"summary"`
	Impact      string           `json:"impact"`
	NodeIDs     []string         `json:"node_ids"`
	SiteIDs     []string         `json:"site_ids"`
	ObservedAt  *time.Time       `json:"observed_at,omitempty"`
	Since       time.Time        `json:"since"`
	Evidence    []HealthEvidence `json:"evidence"`
	ActionURL   string           `json:"action_url"`
	ActionLabel string           `json:"action_label"`
}

type HealthRecovery struct {
	CheckID       string      `json:"check_id"`
	Title         string      `json:"title"`
	PreviousState HealthState `json:"previous_state"`
	Outcome       string      `json:"outcome"`
	StartedAt     time.Time   `json:"started_at"`
	FinishedAt    time.Time   `json:"finished_at"`
	NodeIDs       []string    `json:"node_ids"`
	SiteIDs       []string    `json:"site_ids"`
	ActionURL     string      `json:"action_url"`
}

type SystemHealthSnapshot struct {
	State       HealthState      `json:"state"`
	CollectedAt *time.Time       `json:"collected_at,omitempty"`
	ValidUntil  *time.Time       `json:"valid_until,omitempty"`
	Stale       bool             `json:"stale"`
	Checks      []HealthCheck    `json:"checks"`
	Recoveries  []HealthRecovery `json:"recoveries"`
	Nodes       []HealthResource `json:"nodes"`
	Sites       []HealthResource `json:"sites"`
}

func HealthNeedsAttention(state HealthState) bool {
	return state == HealthCritical || state == HealthWarning || state == HealthUnknown
}

func HealthStateRank(state HealthState) int {
	switch state {
	case HealthCritical:
		return 0
	case HealthWarning:
		return 1
	case HealthUnknown:
		return 2
	case HealthHealthy:
		return 3
	default:
		return 4
	}
}
