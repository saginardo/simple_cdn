package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

// Heartbeats default to 30 seconds. Allow normal scheduling and network jitter
// while requiring an observation made after the installation completed.
const rolloutHeartbeatFreshness = 90 * time.Second

func (s *Server) currentUpgradeRollout(w http.ResponseWriter, r *http.Request) {
	rollout, err := s.Store.LatestUpgradeRollout()
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rollout)
}
func (s *Server) changeUpgradeRollout(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if action != "pause" && action != "resume" && action != "cancel" {
		writeError(w, http.StatusBadRequest, errors.New("invalid rollout action"))
		return
	}
	s.upgradeRolloutMu.Lock()
	defer s.upgradeRolloutMu.Unlock()
	rollout, err := s.Store.ChangeUpgradeRollout(r.PathValue("id"), action)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.audit(r, adminID(r.Context()), action+"_upgrade_rollout", "upgrade_rollout", rollout.ID, rollout.Detail)
	writeJSON(w, http.StatusOK, rollout)
}

func (s *Server) RunUpgradeRollouts(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.reconcileUpgradeRollout(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil && s.Logger != nil {
			s.Logger.Warn("reconcile upgrade rollout", "error", err)
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Server) reconcileUpgradeRollout(ctx context.Context, observedAt time.Time) error {
	s.upgradeRolloutMu.Lock()
	defer s.upgradeRolloutMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := s.Store.ReconcileNodeUpgrades(); err != nil {
		return err
	}
	rollout, err := s.Store.LatestUpgradeRollout()
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if rollout.State != "running" && rollout.State != "paused" {
		return nil
	}
	changed := false
	pause := func(detail string) { rollout.State = "paused"; rollout.Detail = detail; changed = true }
	for i := range rollout.Members {
		member := &rollout.Members[i]
		if member.State != "upgrading" && member.State != "verifying" {
			continue
		}
		task, e := s.Store.NodeUpgradeTask(member.TaskID)
		if e != nil {
			return e
		}
		if task.Status == domain.NodeUpgradeFailed {
			member.State = "failed"
			member.Detail = task.Detail
			pause(member.Name + ": " + task.Detail)
			continue
		}
		if task.Status != domain.NodeUpgradeSucceeded {
			continue
		}
		changed = true
		if member.State != "verifying" || member.VerificationStartedAt == nil {
			member.State = "verifying"
			member.VerificationStartedAt = &observedAt
		}
		ready, detail, e := s.upgradeRolloutNodeReady(ctx, member.NodeID, task, observedAt)
		if e != nil {
			return e
		}
		if !ready {
			member.HealthySince = nil
			member.LastObservedAt = nil
			member.Detail = detail
			if observedAt.Sub(*member.VerificationStartedAt) >= 5*time.Minute || strings.HasPrefix(detail, "健康探测失败") {
				member.State = "failed"
				pause(member.Name + ": " + detail)
			}
			continue
		}
		// A process restart or a long probe gap must earn a fresh continuous window.
		if member.HealthySince == nil || member.LastObservedAt == nil || observedAt.Sub(*member.LastObservedAt) > 30*time.Second || observedAt.Before(*member.LastObservedAt) {
			member.HealthySince = &observedAt
		}
		member.LastObservedAt = &observedAt
		member.Detail = "健康观察中"
		if observedAt.Sub(*member.HealthySince) >= time.Duration(rollout.HealthWindowSeconds)*time.Second {
			member.State = "succeeded"
			member.Detail = "升级和健康观察均已通过"
		}
	}
	if changed {
		if err = s.Store.SaveUpgradeRollout(&rollout); err != nil {
			return err
		}
	}
	if rollout.State == "paused" {
		return nil
	}
	// The binary endpoint and installer are tied to the running control build.
	// Stop if operators replace/promote artifacts during a rollout.
	target, e := s.currentNginxArtifactTarget()
	if e != nil || !strings.EqualFold(rollout.TargetAgentSHA256, strings.TrimSpace(s.EdgeBinarySHA256)) || !strings.EqualFold(rollout.TargetNginxSHA256, target.SHA256) {
		rollout.State = "paused"
		rollout.Detail = "主控升级制品已变化，请取消后重新创建分批升级"
		return s.Store.SaveUpgradeRollout(&rollout)
	}
	for _, member := range rollout.Members {
		if member.State != "pending" {
			continue
		}
		active := 0
		for _, item := range rollout.Members {
			if item.State == "upgrading" || item.State == "verifying" {
				active++
			}
		}
		if active >= rollout.MaxParallel || (rollout.Members[0].NodeID != member.NodeID && rollout.Members[0].State != "succeeded") {
			break
		}
		node, e := s.Store.GetNode(member.NodeID)
		if e != nil || node.LastHeartbeatAt == nil || observedAt.Sub(*node.LastHeartbeatAt) > nodeUpgradeHeartbeatFreshness {
			rollout.State = "paused"
			rollout.Detail = member.Name + ": 节点不存在或心跳已过期"
			return s.Store.SaveUpgradeRollout(&rollout)
		}
		next, e := s.Store.DispatchUpgradeRolloutNode(rollout.ID, member.NodeID, time.Now().UTC().Add(nodeUpgradeTimeout))
		if e != nil {
			rollout.State = "paused"
			rollout.Detail = member.Name + ": " + e.Error()
			return s.Store.SaveUpgradeRollout(&rollout)
		}
		rollout = next
	}
	complete := true
	for _, member := range rollout.Members {
		if member.State != "succeeded" {
			complete = false
		}
	}
	if complete {
		rollout.State = "succeeded"
		rollout.Detail = "全部节点已通过升级和健康观察"
		return s.Store.SaveUpgradeRollout(&rollout)
	}
	return nil
}

func (s *Server) upgradeRolloutNodeReady(ctx context.Context, nodeID string, task domain.NodeUpgradeTask, observedAt time.Time) (bool, string, error) {
	node, err := s.Store.GetNode(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return false, "节点已删除", nil
	}
	if err != nil {
		return false, "", err
	}
	if node.Status != domain.NodeActive && node.Status != domain.NodeDraining {
		return false, "节点状态不可用", nil
	}
	if task.CompletedAt == nil || node.LastHeartbeatAt == nil || node.LastHeartbeatAt.Before(*task.CompletedAt) || observedAt.Sub(*node.LastHeartbeatAt) > rolloutHeartbeatFreshness || node.LastHeartbeatAt.After(observedAt.Add(5*time.Second)) {
		return false, "等待升级完成后的新鲜心跳", nil
	}
	if node.ActiveUpgradeID != "" {
		return false, "等待节点清理本地升级任务", nil
	}
	if !strings.EqualFold(node.AgentSHA256, task.TargetSHA256) || (task.TargetNginxSHA256 != "" && !strings.EqualFold(node.NginxSHA256, task.TargetNginxSHA256)) {
		return false, "节点制品尚未匹配升级目标", nil
	}
	desired, err := s.Store.DesiredVersion(nodeID)
	if err != nil {
		return false, "", err
	}
	if node.AppliedVersion < desired || node.LastError != "" {
		return false, "等待节点确认当前配置且无错误", nil
	}
	health := s.HealthManager
	if health == nil {
		health = &HealthManager{Server: s}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	healthy, detail := health.checkNodeTiered(probeCtx, node, true)
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	if !healthy {
		return false, "健康探测失败: " + detail, nil
	}
	return true, "", nil
}
