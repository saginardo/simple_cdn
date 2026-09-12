package domain

import "time"

// A rollout freezes the fleet and artifacts at creation. Only the canary is
// dispatched until it completes a continuous health observation window.
type NodeUpgradeRollout struct {
	ID                  string                     `json:"id"`
	State               string                     `json:"state"`
	Revision            int                        `json:"revision"`
	MaxParallel         int                        `json:"max_parallel"`
	HealthWindowSeconds int                        `json:"health_window_seconds"`
	TargetAgentSHA256   string                     `json:"target_agent_sha256"`
	TargetNginxSHA256   string                     `json:"target_nginx_sha256,omitempty"`
	Detail              string                     `json:"detail"`
	Members             []NodeUpgradeRolloutMember `json:"members"`
	CreatedAt           time.Time                  `json:"created_at"`
	UpdatedAt           time.Time                  `json:"updated_at"`
}

type NodeUpgradeRolloutMember struct {
	NodeID                string     `json:"node_id"`
	Name                  string     `json:"name"`
	State                 string     `json:"state"`
	TaskID                string     `json:"task_id,omitempty"`
	Detail                string     `json:"detail,omitempty"`
	HealthySince          *time.Time `json:"healthy_since,omitempty"`
	LastObservedAt        *time.Time `json:"last_observed_at,omitempty"`
	VerificationStartedAt *time.Time `json:"verification_started_at,omitempty"`
}
