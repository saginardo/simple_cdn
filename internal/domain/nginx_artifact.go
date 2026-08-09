package domain

import "time"

type NginxArtifactState string

const (
	NginxArtifactCandidate NginxArtifactState = "candidate"
	NginxArtifactCurrent   NginxArtifactState = "current"
	NginxArtifactRetired   NginxArtifactState = "retired"
)

// NginxArtifact describes a validated, content-addressed managed Nginx bundle.
// The file location is derived from SHA256 and intentionally stays out of the
// database so deployment paths can move without rewriting catalog records.
type NginxArtifact struct {
	SHA256            string             `json:"sha256"`
	Version           string             `json:"version"`
	State             NginxArtifactState `json:"state"`
	ReleaseTag        string             `json:"release_tag"`
	SourceURL         string             `json:"source_url"`
	OfficialSourceURL string             `json:"official_source_url"`
	SourceSHA256      string             `json:"source_sha256"`
	BuildCommit       string             `json:"build_commit"`
	SizeBytes         int64              `json:"size_bytes"`
	DownloadedAt      time.Time          `json:"downloaded_at"`
	PromotedAt        *time.Time         `json:"promoted_at,omitempty"`
}
