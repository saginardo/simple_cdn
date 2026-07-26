package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
	"simple_cdn/internal/logstore"
	"simple_cdn/internal/store"
)

const monitoringStaleAfter = 75 * time.Second

const monitoringNotificationCooldown = 5 * time.Minute

type monitoringOverviewResponse struct {
	Targets          []domain.MonitoringTarget `json:"targets"`
	Nodes            []monitoringNodeResponse  `json:"nodes"`
	IntervalSeconds  int                       `json:"interval_seconds"`
	AttemptsPerRound int                       `json:"attempts_per_round"`
	HealthyScore     int                       `json:"healthy_score"`
	AutoPauseAfter   int                       `json:"auto_pause_after"`
}

type monitoringNodeResponse struct {
	NodeID              string                    `json:"node_id"`
	Name                string                    `json:"name"`
	PublicIPv4          string                    `json:"public_ipv4"`
	Status              domain.NodeStatus         `json:"status"`
	MonitorAutoPaused   bool                      `json:"monitor_auto_paused"`
	Capable             bool                      `json:"capable"`
	Score               *int                      `json:"score,omitempty"`
	SuccessRate         *float64                  `json:"success_rate,omitempty"`
	AverageLatencyMS    *float64                  `json:"average_latency_ms,omitempty"`
	ConsecutiveAbnormal int                       `json:"consecutive_abnormal"`
	LastCheckedAt       *time.Time                `json:"last_checked_at,omitempty"`
	Stale               bool                      `json:"stale"`
	Results             []monitoringProbeResponse `json:"results"`
}

type monitoringProbeResponse struct {
	TargetID           string    `json:"target_id"`
	TargetName         string    `json:"target_name"`
	Address            string    `json:"address"`
	Attempts           int       `json:"attempts"`
	SuccessfulAttempts int       `json:"successful_attempts"`
	AverageLatencyMS   float64   `json:"average_latency_ms"`
	Error              string    `json:"error,omitempty"`
	CheckedAt          time.Time `json:"checked_at"`
}

type smartRoutingOverviewResponse struct {
	Timezone string                     `json:"timezone"`
	Nodes    []smartRoutingNodeResponse `json:"nodes"`
}

type smartRoutingNodeResponse struct {
	NodeID     string                       `json:"node_id"`
	Name       string                       `json:"name"`
	PublicIPv4 string                       `json:"public_ipv4"`
	Status     domain.NodeStatus            `json:"status"`
	Capable    bool                         `json:"capable"`
	AutoPaused bool                         `json:"auto_paused"`
	Enabled    bool                         `json:"enabled"`
	BlockedBy  []string                     `json:"blocked_by"`
	Score      smartRoutingScoreResponse    `json:"score"`
	Schedule   smartRoutingScheduleResponse `json:"schedule"`
	UpdatedAt  time.Time                    `json:"updated_at"`
}

type smartRoutingScoreResponse struct {
	Enabled           bool       `json:"enabled"`
	PauseBelowScore   int        `json:"pause_below_score"`
	PauseAfterRounds  int        `json:"pause_after_rounds"`
	ResumeAtScore     int        `json:"resume_at_score"`
	ResumeAfterRounds int        `json:"resume_after_rounds"`
	Gate              string     `json:"gate"`
	CurrentScore      *int       `json:"current_score,omitempty"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	Stale             bool       `json:"stale"`
	LowStreak         int        `json:"low_streak"`
	RecoveryStreak    int        `json:"recovery_streak"`
}

type smartRoutingScheduleResponse struct {
	Enabled          bool                       `json:"enabled"`
	Windows          []store.SmartRoutingWindow `json:"windows"`
	Gate             string                     `json:"gate"`
	NextTransitionAt *time.Time                 `json:"next_transition_at,omitempty"`
}

func (s *Server) smartRoutingOverview(response http.ResponseWriter, _ *http.Request) {
	overview, err := s.loadSmartRoutingOverview(time.Now().UTC())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, overview)
}

func (s *Server) loadSmartRoutingOverview(at time.Time) (smartRoutingOverviewResponse, error) {
	states, err := s.Store.ListSmartRoutingNodeStates()
	if err != nil {
		return smartRoutingOverviewResponse{}, err
	}
	nodes := make([]smartRoutingNodeResponse, 0, len(states))
	for _, state := range states {
		if state.Node.Status == domain.NodeUninstalled {
			continue
		}
		policy := state.Policy
		item := smartRoutingNodeResponse{
			NodeID: state.Node.ID, Name: state.Node.Name, PublicIPv4: state.Node.PublicIPv4,
			Status: state.Node.Status, Capable: slices.Contains(state.Node.Capabilities, domain.EdgeCapabilityTCPMonitoring),
			AutoPaused: state.Node.MonitorAutoPaused, Enabled: policy.Enabled, BlockedBy: policy.BlockedBy(), UpdatedAt: policy.UpdatedAt,
			Score: smartRoutingScoreResponse{
				Enabled: policy.ScoreEnabled, PauseBelowScore: policy.ScorePauseBelow,
				PauseAfterRounds: policy.ScorePauseRounds, ResumeAtScore: policy.ScoreResumeAt,
				ResumeAfterRounds: policy.ScoreResumeRounds, Gate: policy.ScoreGate,
				CurrentScore: state.CurrentScore, LastCheckedAt: state.LastCheckedAt,
				Stale:     state.LastCheckedAt == nil || state.LastCheckedAt.Before(at.Add(-monitoringStaleAfter)),
				LowStreak: policy.ScoreLowStreak, RecoveryStreak: policy.ScoreRecoveryStreak,
			},
			Schedule: smartRoutingScheduleResponse{
				Enabled: policy.ScheduleEnabled, Windows: policy.ScheduleWindows, Gate: policy.ScheduleGate,
			},
		}
		if policy.ScheduleEnabled {
			item.Schedule.NextTransitionAt = store.SmartRoutingNextTransition(at, policy.ScheduleWindows)
		}
		nodes = append(nodes, item)
	}
	return smartRoutingOverviewResponse{Timezone: store.SmartRoutingTimezone, Nodes: nodes}, nil
}

func (s *Server) updateNodeSmartRouting(response http.ResponseWriter, request *http.Request) {
	var input store.SmartRoutingConfig
	if !readJSON(response, request, &input) {
		return
	}
	nodeID := request.PathValue("id")
	outcome, err := s.Store.UpdateSmartRoutingPolicy(nodeID, input, time.Now().UTC(), monitoringStaleAfter)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	actor := adminID(request.Context())
	if outcome.ConfigChanged {
		detail := fmt.Sprintf("enabled=%t score=%t pause<%d/%d resume>=%d/%d schedule=%t windows=%d",
			outcome.Policy.Enabled, outcome.Policy.ScoreEnabled, outcome.Policy.ScorePauseBelow,
			outcome.Policy.ScorePauseRounds, outcome.Policy.ScoreResumeAt, outcome.Policy.ScoreResumeRounds,
			outcome.Policy.ScheduleEnabled, len(outcome.Policy.ScheduleWindows))
		s.audit(request, actor, "update_smart_routing", "node", nodeID, detail)
	}
	s.auditSmartRoutingTransition(request, actor, "configuration", outcome.Transition)
	if outcome.Transition.ScoreGateChanged &&
		(outcome.Transition.PreviousScoreGate == store.SmartRoutingScoreBlocked || outcome.Transition.ScoreGate == store.SmartRoutingScoreBlocked) {
		s.notifySmartRoutingConfigurationTransition(request.Context(), nodeID, outcome)
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) notifySmartRoutingConfigurationTransition(ctx context.Context, nodeID string, outcome store.SmartRoutingUpdateOutcome) {
	if s.Notifier == nil {
		return
	}
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("load node for smart routing configuration notification", "node_id", nodeID, "error", err)
		}
		return
	}
	details := []integrations.NotificationDetail{
		{Label: "节点", Value: node.Name},
		{Label: "公网地址", Value: node.PublicIPv4},
		{Label: "暂停规则", Value: fmt.Sprintf("低于 %d 分连续 %d 轮", outcome.Policy.ScorePauseBelow, outcome.Policy.ScorePauseRounds)},
		{Label: "恢复规则", Value: fmt.Sprintf("达到 %d 分连续 %d 轮", outcome.Policy.ScoreResumeAt, outcome.Policy.ScoreResumeRounds)},
	}
	notification := integrations.Notification{
		Category: integrations.NotificationCategoryMonitoring, OccurredAt: time.Now().UTC(),
		Key: "monitoring:node:" + nodeID, Cooldown: monitoringNotificationCooldown, Details: details,
	}
	if outcome.Transition.ScoreGate == store.SmartRoutingScoreBlocked {
		notification.Severity = integrations.NotificationSeverityError
		notification.Subject = "[CDN] 评分规则更新，节点已触发阻断"
		if outcome.Transition.StatusChanged && outcome.Transition.NodeStatus == domain.NodeDraining {
			notification.Message = "评分规则更新后，当前评分满足暂停条件，智能路由已将节点移出边缘调度。"
		} else {
			notification.Message = "评分规则更新后，当前评分满足暂停条件；节点当前未由智能路由切换状态。"
		}
		notification.SuppressUntilResolved = true
	} else {
		notification.Severity = integrations.NotificationSeveritySuccess
		if !outcome.Policy.ScoreEnabled {
			notification.Subject = "[CDN] 评分门控已关闭，异常状态已解除"
			notification.Message = "管理员关闭了评分门控，原评分异常事件已解除。"
		} else if slices.Contains(outcome.Transition.BlockedBy, "schedule") {
			notification.Subject = "[CDN] 评分门控已恢复，仍受时间规则限制"
			notification.Message = "评分规则更新后，当前评分满足恢复条件；节点仍处于时间规则关闭区间。"
		} else {
			notification.Subject = "[CDN] 评分门控已恢复，节点可加入调度"
			notification.Message = "评分规则更新后，当前评分满足恢复条件，评分异常事件已解除。"
		}
		notification.Resolved = true
		notification.NotifyOnResolve = true
	}
	if err := integrations.SendNotification(ctx, s.Notifier, notification); err != nil && s.Logger != nil {
		s.Logger.Warn("send smart routing configuration notification", "node_id", nodeID, "score_gate", outcome.Transition.ScoreGate, "error", err)
	}
}

func (s *Server) auditSmartRoutingTransition(request *http.Request, actor, trigger string, transition store.SmartRoutingTransition) {
	detail := fmt.Sprintf("trigger=%s status=%s blocked_by=%s", trigger, transition.NodeStatus, strings.Join(transition.BlockedBy, ","))
	if transition.ScoreGateChanged {
		action := "smart_routing_score_pending"
		if transition.ScoreGate == store.SmartRoutingScoreBlocked {
			action = "smart_routing_score_blocked"
		} else if transition.ScoreGate == store.SmartRoutingScoreAllowed {
			action = "smart_routing_score_allowed"
		}
		s.audit(request, actor, action, "node", transition.NodeID, detail)
	}
	if transition.ScheduleGateChanged {
		action := "smart_routing_schedule_opened"
		if transition.ScheduleGate == store.SmartRoutingScheduleClosed {
			action = "smart_routing_schedule_closed"
		}
		s.audit(request, actor, action, "node", transition.NodeID, detail)
	}
	if transition.StatusChanged {
		action := "smart_routing_auto_resume"
		if transition.NodeStatus == domain.NodeDraining {
			action = "smart_routing_auto_pause"
		}
		s.audit(request, actor, action, "node", transition.NodeID, detail)
	}
}

func (s *Server) monitoringOverview(response http.ResponseWriter, _ *http.Request) {
	targets, err := s.Store.ListMonitoringTargets(false)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	nodes, err := s.Store.ListNodes()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	statuses, err := s.Store.ListNodeMonitoringStatuses()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	results, err := s.Store.ListMonitoringProbeSnapshots()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	statusByNode := make(map[string]store.NodeMonitoringStatus, len(statuses))
	for _, status := range statuses {
		statusByNode[status.NodeID] = status
	}
	targetByID := make(map[string]domain.MonitoringTarget, len(targets))
	for _, target := range targets {
		targetByID[target.ID] = target
	}
	resultsByNode := make(map[string][]monitoringProbeResponse)
	for _, result := range results {
		target, found := targetByID[result.TargetID]
		if !found {
			continue
		}
		resultsByNode[result.NodeID] = append(resultsByNode[result.NodeID], monitoringProbeResponse{
			TargetID: result.TargetID, TargetName: target.Name, Address: target.Address, Attempts: result.Attempts,
			SuccessfulAttempts: result.SuccessfulAttempts, AverageLatencyMS: result.AverageLatencyMS,
			Error: result.Error, CheckedAt: result.CheckedAt,
		})
	}
	now := time.Now().UTC()
	responseNodes := make([]monitoringNodeResponse, 0, len(nodes))
	for _, node := range nodes {
		if node.Status == domain.NodeUninstalled {
			continue
		}
		item := monitoringNodeResponse{
			NodeID: node.ID, Name: node.Name, PublicIPv4: node.PublicIPv4, Status: node.Status,
			MonitorAutoPaused: node.MonitorAutoPaused,
			Capable:           slices.Contains(node.Capabilities, domain.EdgeCapabilityTCPMonitoring),
			Results:           resultsByNode[node.ID],
		}
		if item.Results == nil {
			item.Results = []monitoringProbeResponse{}
		}
		if status, found := statusByNode[node.ID]; found {
			item.Score = &status.Score
			item.SuccessRate = &status.SuccessRate
			item.AverageLatencyMS = &status.AverageLatencyMS
			item.ConsecutiveAbnormal = status.ConsecutiveAbnormal
			item.LastCheckedAt = status.LastCheckedAt
			item.Stale = status.LastCheckedAt == nil || status.LastCheckedAt.Before(now.Add(-monitoringStaleAfter))
		}
		responseNodes = append(responseNodes, item)
	}
	writeJSON(response, http.StatusOK, monitoringOverviewResponse{
		Targets: targets, Nodes: responseNodes, IntervalSeconds: 30, AttemptsPerRound: 3,
		HealthyScore: store.MonitoringHealthyScore, AutoPauseAfter: store.MonitoringAutoPauseAfter,
	})
}

type monitoringHistoryPreset struct {
	duration time.Duration
	bucket   time.Duration
}

var monitoringHistoryPresets = map[string]monitoringHistoryPreset{
	"1h":  {duration: time.Hour, bucket: 30 * time.Second},
	"6h":  {duration: 6 * time.Hour, bucket: 2 * time.Minute},
	"12h": {duration: 12 * time.Hour, bucket: 5 * time.Minute},
	"24h": {duration: 24 * time.Hour, bucket: 10 * time.Minute},
	"7d":  {duration: 7 * 24 * time.Hour, bucket: time.Hour},
}

type monitoringHistoryResponse struct {
	Available         bool                              `json:"available"`
	UnavailableReason string                            `json:"unavailable_reason,omitempty"`
	Node              monitoringHistoryNodeResponse     `json:"node"`
	Range             string                            `json:"range"`
	From              time.Time                         `json:"from"`
	To                time.Time                         `json:"to"`
	BucketSeconds     int                               `json:"bucket_seconds"`
	Series            []monitoringHistorySeriesResponse `json:"series"`
}

type monitoringHistoryNodeResponse struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	PublicIPv4        string            `json:"public_ipv4"`
	Status            domain.NodeStatus `json:"status"`
	MonitorAutoPaused bool              `json:"monitor_auto_paused"`
}

type monitoringHistorySeriesResponse struct {
	TargetID string                           `json:"target_id"`
	Name     string                           `json:"name"`
	Address  string                           `json:"address"`
	Points   []monitoringHistoryPointResponse `json:"points"`
}

type monitoringHistoryPointResponse struct {
	Time               time.Time `json:"time"`
	Attempts           uint64    `json:"attempts"`
	SuccessfulAttempts uint64    `json:"successful_attempts"`
	SuccessRate        float64   `json:"success_rate"`
	AverageLatencyMS   *float64  `json:"average_latency_ms"`
	FailedRounds       uint64    `json:"failed_rounds"`
}

func (s *Server) monitoringNodeHistory(response http.ResponseWriter, request *http.Request) {
	node, err := s.Store.GetNode(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	rangeName := strings.TrimSpace(request.URL.Query().Get("range"))
	if rangeName == "" {
		rangeName = "24h"
	}
	preset, found := monitoringHistoryPresets[rangeName]
	if !found {
		writeError(response, http.StatusBadRequest, errors.New("range must be one of 1h, 6h, 12h, 24h, or 7d"))
		return
	}
	to := time.Now().UTC()
	from := to.Add(-preset.duration)
	result := monitoringHistoryResponse{
		Available: s.MonitoringHistory != nil,
		Node: monitoringHistoryNodeResponse{
			ID: node.ID, Name: node.Name, PublicIPv4: node.PublicIPv4,
			Status: node.Status, MonitorAutoPaused: node.MonitorAutoPaused,
		},
		Range: rangeName, From: from, To: to, BucketSeconds: int(preset.bucket / time.Second),
		Series: []monitoringHistorySeriesResponse{},
	}
	if s.MonitoringHistory == nil {
		result.UnavailableReason = "ClickHouse 历史存储未启用"
		writeJSON(response, http.StatusOK, result)
		return
	}
	buckets, err := s.MonitoringHistory.MonitoringHistory(request.Context(), logstore.MonitoringHistoryQuery{
		NodeID: node.ID, From: from, To: to, Bucket: preset.bucket,
	})
	if err != nil {
		result.Available = false
		result.UnavailableReason = "历史拨测数据暂时不可用"
		if s.Logger != nil {
			s.Logger.Warn("query monitoring history", "node_id", node.ID, "range", rangeName, "error", err)
		}
		writeJSON(response, http.StatusOK, result)
		return
	}
	targets, err := s.Store.ListMonitoringTargets(false)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	currentTargets := make(map[string]domain.MonitoringTarget, len(targets))
	for _, target := range targets {
		currentTargets[target.ID] = target
	}
	seriesByTarget := make(map[string]*monitoringHistorySeriesResponse)
	for _, bucket := range buckets {
		series := seriesByTarget[bucket.TargetID]
		if series == nil {
			name, address := bucket.TargetName, bucket.TargetAddress
			if target, current := currentTargets[bucket.TargetID]; current {
				name, address = target.Name, target.Address
			}
			created := &monitoringHistorySeriesResponse{
				TargetID: bucket.TargetID, Name: name, Address: address,
				Points: []monitoringHistoryPointResponse{},
			}
			seriesByTarget[bucket.TargetID] = created
			series = created
		}
		successRate := 0.0
		if bucket.Attempts != 0 {
			successRate = 100 * float64(bucket.SuccessfulAttempts) / float64(bucket.Attempts)
		}
		series.Points = append(series.Points, monitoringHistoryPointResponse{
			Time: bucket.Time, Attempts: bucket.Attempts, SuccessfulAttempts: bucket.SuccessfulAttempts,
			SuccessRate: successRate, AverageLatencyMS: bucket.AverageLatencyMS, FailedRounds: bucket.FailedRounds,
		})
	}
	for _, series := range seriesByTarget {
		result.Series = append(result.Series, *series)
	}
	slices.SortFunc(result.Series, func(left, right monitoringHistorySeriesResponse) int {
		if compared := strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name)); compared != 0 {
			return compared
		}
		return strings.Compare(left.TargetID, right.TargetID)
	})
	writeJSON(response, http.StatusOK, result)
}

type createMonitoringTargetRequest struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

func (s *Server) createMonitoringTarget(response http.ResponseWriter, request *http.Request) {
	var input createMonitoringTargetRequest
	if !readJSON(response, request, &input) {
		return
	}
	target, err := s.Store.CreateMonitoringTarget(input.Name, input.Address)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "create_monitoring_target", "monitoring_target", target.ID, target.Name+" "+target.Address)
	writeJSON(response, http.StatusCreated, target)
}

type updateMonitoringTargetRequest struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
}

func (s *Server) updateMonitoringTarget(response http.ResponseWriter, request *http.Request) {
	var input updateMonitoringTargetRequest
	if !readJSON(response, request, &input) {
		return
	}
	if input.Name == nil && input.Enabled == nil {
		writeError(response, http.StatusBadRequest, errors.New("name or enabled is required"))
		return
	}
	target, err := s.Store.UpdateMonitoringTarget(request.PathValue("id"), input.Name, input.Enabled)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "update_monitoring_target", "monitoring_target", target.ID, target.Name+" "+target.Address)
	writeJSON(response, http.StatusOK, target)
}

func (s *Server) deleteMonitoringTarget(response http.ResponseWriter, request *http.Request) {
	if err := s.Store.DeleteMonitoringTarget(request.PathValue("id")); err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "delete_monitoring_target", "monitoring_target", request.PathValue("id"), "")
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) edgeMonitoringTargets(response http.ResponseWriter, request *http.Request) {
	targets, err := s.Store.ListMonitoringTargets(true)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	revision := monitoringTargetsRevision(targets)
	if requestHasRevision(request, revision) {
		writeRevisionNotModified(response, revision)
		return
	}
	response.Header().Set("ETag", revisionETag(revision))
	writeJSON(response, http.StatusOK, targets)
}

type monitoringReportRequest struct {
	Results []domain.MonitoringProbeResult `json:"results"`
}

func (s *Server) edgeMonitoringReport(response http.ResponseWriter, request *http.Request) {
	var input monitoringReportRequest
	if !readJSON(response, request, &input) {
		return
	}
	if len(input.Results) == 0 || len(input.Results) > domain.MaxMonitoringTargets {
		writeError(response, http.StatusBadRequest, errors.New("monitoring results must contain every enabled target"))
		return
	}
	now := time.Now().UTC()
	for index := range input.Results {
		input.Results[index].CheckedAt = input.Results[index].CheckedAt.UTC()
		if !domain.ValidMonitoringProbeResult(input.Results[index]) || input.Results[index].CheckedAt.After(now.Add(maxEdgeReportClockSkew)) {
			writeError(response, http.StatusBadRequest, errors.New("monitoring result is invalid"))
			return
		}
	}
	nodeID := edgeNodeID(request.Context())
	outcome, err := s.Store.RecordMonitoringRound(nodeID, input.Results)
	if errors.Is(err, store.ErrMonitoringReportStale) {
		writeJSON(response, http.StatusOK, map[string]bool{"accepted": false})
		return
	}
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.enqueueMonitoringHistory(nodeID, input.Results)
	if outcome.Transition.ScoreGateChanged || outcome.Transition.StatusChanged {
		s.auditSmartRoutingTransition(request, "edge:"+nodeID, "score report: "+outcome.Status.String(), outcome.Transition)
	}
	scoreIncidentTransition := outcome.Transition.ScoreGateChanged &&
		(outcome.Transition.PreviousScoreGate == store.SmartRoutingScoreBlocked || outcome.Transition.ScoreGate == store.SmartRoutingScoreBlocked)
	if scoreIncidentTransition || outcome.AutoPaused && outcome.Transition.ScoreGate == store.SmartRoutingScoreBlocked {
		s.notifyMonitoringTransition(request.Context(), nodeID, input.Results, outcome)
	}
	writeJSON(response, http.StatusOK, map[string]bool{"accepted": true})
}

func (s *Server) notifyMonitoringTransition(ctx context.Context, nodeID string, results []domain.MonitoringProbeResult, outcome store.MonitoringRoundOutcome) {
	if s.Notifier == nil {
		return
	}
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("load node for monitoring notification", "node_id", nodeID, "error", err)
		}
		return
	}
	policy, err := s.Store.SmartRoutingPolicy(nodeID)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("load smart routing policy for monitoring notification", "node_id", nodeID, "error", err)
		}
		return
	}
	targets, err := s.Store.ListMonitoringTargets(false)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("load targets for monitoring notification", "node_id", nodeID, "error", err)
		}
		return
	}
	targetByID := make(map[string]domain.MonitoringTarget, len(targets))
	for _, target := range targets {
		targetByID[target.ID] = target
	}
	details := []integrations.NotificationDetail{
		{Label: "节点", Value: node.Name},
		{Label: "公网地址", Value: node.PublicIPv4},
		{Label: "综合评分", Value: fmt.Sprintf("%d / 100", outcome.Status.Score)},
		{Label: "成功率", Value: fmt.Sprintf("%.1f%%", outcome.Status.SuccessRate)},
		{Label: "平均延迟", Value: fmt.Sprintf("%.1f ms", outcome.Status.AverageLatencyMS)},
		{Label: "暂停规则", Value: fmt.Sprintf("低于 %d 分连续 %d 轮", policy.ScorePauseBelow, policy.ScorePauseRounds)},
		{Label: "恢复规则", Value: fmt.Sprintf("达到 %d 分连续 %d 轮", policy.ScoreResumeAt, policy.ScoreResumeRounds)},
	}
	failedTargets := 0
	for _, result := range results {
		if result.SuccessfulAttempts == result.Attempts && strings.TrimSpace(result.Error) == "" {
			continue
		}
		if failedTargets >= 12 {
			break
		}
		name := result.TargetID
		if target, found := targetByID[result.TargetID]; found {
			name = target.Name
		}
		detail := fmt.Sprintf("%d/%d 次成功", result.SuccessfulAttempts, result.Attempts)
		if strings.TrimSpace(result.Error) != "" {
			detail += "；" + strings.TrimSpace(result.Error)
		}
		details = append(details, integrations.NotificationDetail{Label: "目标 " + name, Value: detail})
		failedTargets++
	}
	notification := integrations.Notification{
		Category:   integrations.NotificationCategoryMonitoring,
		OccurredAt: time.Now().UTC(),
		Key:        "monitoring:node:" + nodeID,
		Cooldown:   monitoringNotificationCooldown,
		Details:    details,
	}
	if outcome.Transition.ScoreGate == store.SmartRoutingScoreBlocked {
		notification.Severity = integrations.NotificationSeverityError
		if outcome.Transition.StatusChanged && outcome.NodeStatus == domain.NodeDraining {
			notification.Subject = "[CDN] 节点评分异常，已移出调度"
			notification.Message = "节点评分触发暂停规则，智能路由已将该节点移出边缘调度。"
		} else if slices.Contains(outcome.Transition.BlockedBy, "schedule") {
			notification.Subject = "[CDN] 节点评分异常，时间规则仍阻止调度"
			notification.Message = "节点评分已触发暂停规则；节点同时处于时间规则关闭区间，仍不会加入边缘调度。"
		} else {
			notification.Subject = "[CDN] 节点评分异常，智能路由已阻止调度"
			notification.Message = "节点评分触发暂停规则，当前节点状态未由智能路由切换，请检查人工调度状态。"
		}
		notification.SuppressUntilResolved = true
	} else {
		notification.Severity = integrations.NotificationSeveritySuccess
		if outcome.NodeStatus == domain.NodeActive {
			notification.Subject = "[CDN] 节点评分恢复，已加入调度"
			notification.Message = "节点评分已满足恢复规则，智能路由已将该节点重新加入边缘调度。"
		} else if slices.Contains(outcome.Transition.BlockedBy, "schedule") {
			notification.Subject = "[CDN] 节点评分恢复，仍受时间规则限制"
			notification.Message = "节点评分已恢复，但当前不在允许调度的时间窗口内，节点仍处于暂停调度状态。"
		} else {
			notification.Subject = "[CDN] 节点评分恢复，仍处于人工暂停"
			notification.Message = "节点评分已恢复，但节点当前状态不允许加入调度，请检查人工调度状态。"
		}
		notification.Resolved = true
		notification.NotifyOnResolve = true
	}
	if err := integrations.SendNotification(ctx, s.Notifier, notification); err != nil && s.Logger != nil {
		s.Logger.Warn("send monitoring notification", "node_id", nodeID, "status", outcome.NodeStatus, "error", err)
	}
}

func (s *Server) enqueueMonitoringHistory(nodeID string, results []domain.MonitoringProbeResult) {
	if s.MonitoringWriter == nil {
		return
	}
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		s.logMonitoringHistoryEnqueueError(nodeID, err)
		return
	}
	targets, err := s.Store.ListMonitoringTargets(false)
	if err != nil {
		s.logMonitoringHistoryEnqueueError(nodeID, err)
		return
	}
	targetByID := make(map[string]domain.MonitoringTarget, len(targets))
	for _, target := range targets {
		targetByID[target.ID] = target
	}
	samples := make([]logstore.MonitoringSample, 0, len(results))
	for _, result := range results {
		target, found := targetByID[result.TargetID]
		if !found {
			continue
		}
		samples = append(samples, logstore.MonitoringSample{
			NodeID: node.ID, NodeName: node.Name,
			TargetID: target.ID, TargetName: target.Name, TargetAddress: target.Address,
			Attempts: result.Attempts, SuccessfulAttempts: result.SuccessfulAttempts,
			AverageLatencyMS: result.AverageLatencyMS, Error: result.Error, CheckedAt: result.CheckedAt,
		})
	}
	if len(samples) != len(results) {
		s.logMonitoringHistoryEnqueueError(nodeID, errors.New("monitoring target metadata changed before history enqueue"))
		return
	}
	if !s.MonitoringWriter.EnqueueMonitoring(samples) && s.Logger != nil {
		s.Logger.Warn("monitoring history queue rejected samples", "node_id", nodeID, "samples", len(samples))
	}
}

func (s *Server) logMonitoringHistoryEnqueueError(nodeID string, err error) {
	if s.Logger != nil {
		s.Logger.Warn("prepare monitoring history samples", "node_id", nodeID, "error", err)
	}
}
