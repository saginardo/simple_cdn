package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func (s *Server) edgeControlManifest(nodeID string) (domain.EdgeControlManifest, error) {
	desiredVersion, err := s.Store.DesiredVersion(nodeID)
	if err != nil {
		return domain.EdgeControlManifest{}, err
	}
	targets, err := s.Store.ListMonitoringTargets(true)
	if err != nil {
		return domain.EdgeControlManifest{}, err
	}
	securityRevision, err := s.cachedEdgeSecurityRevision()
	if err != nil {
		return domain.EdgeControlManifest{}, err
	}
	upgradeTaskID := ""
	if instruction, instructionErr := s.Store.NodeUpgradeInstruction(nodeID); instructionErr == nil {
		upgradeTaskID = instruction.TaskID
	} else if !errors.Is(instructionErr, store.ErrNotFound) {
		return domain.EdgeControlManifest{}, instructionErr
	}
	return domain.EdgeControlManifest{
		DesiredStateVersion: desiredVersion,
		MonitoringRevision:  monitoringTargetsRevision(targets),
		SecurityRevision:    securityRevision,
		UpgradeTaskID:       upgradeTaskID,
		AccessLogGzip:       true,
	}, nil
}

func (s *Server) cachedEdgeSecurityRevision() (string, error) {
	s.edgeSecurityRevisionMu.Lock()
	defer s.edgeSecurityRevisionMu.Unlock()
	now := time.Now().UTC()
	if s.edgeSecurityRevisionSet && (s.edgeSecurityExpiresAt.IsZero() || now.Before(s.edgeSecurityExpiresAt)) {
		return s.edgeSecurityRevision, nil
	}
	bans, err := s.Store.ListActiveSecurityBans()
	if err != nil {
		return "", err
	}
	s.cacheEdgeSecurityRevisionLocked(bans)
	return s.edgeSecurityRevision, nil
}

func (s *Server) cacheEdgeSecurityRevisionLocked(bans []domain.SecurityBan) string {
	s.edgeSecurityRevision = securityBansRevision(bans)
	s.edgeSecurityExpiresAt = time.Time{}
	for _, ban := range bans {
		if s.edgeSecurityExpiresAt.IsZero() || ban.ExpiresAt.Before(s.edgeSecurityExpiresAt) {
			s.edgeSecurityExpiresAt = ban.ExpiresAt
		}
	}
	s.edgeSecurityRevisionSet = true
	return s.edgeSecurityRevision
}

func (s *Server) invalidateEdgeSecurityRevision() {
	s.edgeSecurityRevisionMu.Lock()
	s.edgeSecurityRevisionSet = false
	s.edgeSecurityRevisionMu.Unlock()
}

func monitoringTargetsRevision(targets []domain.MonitoringTarget) string {
	ordered := append([]domain.MonitoringTarget(nil), targets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	return revisionDigest(ordered)
}

func securityBansRevision(bans []domain.SecurityBan) string {
	type revisionBan struct {
		IP        string `json:"ip"`
		ExpiresAt string `json:"expires_at"`
	}
	ordered := make([]revisionBan, 0, len(bans))
	for _, ban := range bans {
		ordered = append(ordered, revisionBan{IP: ban.IP, ExpiresAt: ban.ExpiresAt.UTC().Format(time.RFC3339Nano)})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].IP != ordered[j].IP {
			return ordered[i].IP < ordered[j].IP
		}
		return ordered[i].ExpiresAt < ordered[j].ExpiresAt
	})
	return revisionDigest(ordered)
}

func revisionDigest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func revisionETag(revision string) string {
	return `"` + revision + `"`
}

func requestHasRevision(request *http.Request, revision string) bool {
	if revision == "" {
		return false
	}
	wanted := revisionETag(revision)
	for _, candidate := range strings.Split(request.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimSpace(strings.TrimPrefix(candidate, "W/"))
		if candidate == "*" || candidate == wanted {
			return true
		}
	}
	return false
}

func writeRevisionNotModified(response http.ResponseWriter, revision string) {
	response.Header().Set("ETag", revisionETag(revision))
	response.WriteHeader(http.StatusNotModified)
}
