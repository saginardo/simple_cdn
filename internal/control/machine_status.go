package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

const (
	machineStatusSSEKeepAlive       = 15 * time.Second
	defaultMachineStatusDemandGrace = 15 * time.Second
)

type machineStatusCapabilityProfile struct {
	supported bool
	stream    bool
	adaptive  bool
}

func machineStatusCapabilityProfileFor(capabilities []string) machineStatusCapabilityProfile {
	var profile machineStatusCapabilityProfile
	for _, capability := range capabilities {
		switch strings.TrimSpace(capability) {
		case domain.EdgeCapabilityMachineStatus:
			profile.supported = true
		case domain.EdgeCapabilityMachineStatusStream:
			profile.stream = true
		case domain.EdgeCapabilityMachineStatusAdaptive:
			profile.adaptive = true
		}
	}
	return profile
}

func (s *Server) seedMachineStatusCapabilityProfile(nodeID string, capabilities []string) machineStatusCapabilityProfile {
	profile := machineStatusCapabilityProfileFor(capabilities)
	s.machineStatusMu.Lock()
	defer s.machineStatusMu.Unlock()
	if current, found := s.machineStatusCapabilities[nodeID]; found {
		return current
	}
	if s.machineStatusCapabilities == nil {
		s.machineStatusCapabilities = make(map[string]machineStatusCapabilityProfile)
	}
	s.machineStatusCapabilities[nodeID] = profile
	return profile
}

func (s *Server) currentMachineStatusCapabilityProfile(nodeID string, fallback machineStatusCapabilityProfile) machineStatusCapabilityProfile {
	s.machineStatusMu.RLock()
	profile, found := s.machineStatusCapabilities[nodeID]
	s.machineStatusMu.RUnlock()
	if found {
		return profile
	}
	return fallback
}

func (s *Server) updateMachineStatusCapabilityProfile(nodeID string, capabilities []string) {
	profile := machineStatusCapabilityProfileFor(capabilities)
	s.machineStatusMu.Lock()
	if s.machineStatusCapabilities == nil {
		s.machineStatusCapabilities = make(map[string]machineStatusCapabilityProfile)
	}
	current, found := s.machineStatusCapabilities[nodeID]
	if found && current == profile {
		s.machineStatusMu.Unlock()
		return
	}
	s.machineStatusCapabilities[nodeID] = profile
	subscribers := s.machineStatusSubscribersLocked(nodeID)
	notifyMachineStatusSubscribers(subscribers)
	s.machineStatusMu.Unlock()
}

func (s *Server) edgeMachineStatus(response http.ResponseWriter, request *http.Request) {
	var report domain.MachineStatus
	if !readJSON(response, request, &report) {
		return
	}
	if !domain.ValidMachineStatus(report) {
		writeError(response, http.StatusBadRequest, errors.New("machine_status is invalid"))
		return
	}
	report.CollectedAt = report.CollectedAt.UTC()
	accepted := false
	if !report.CollectedAt.After(time.Now().UTC().Add(maxEdgeReportClockSkew)) {
		accepted = s.recordNodeMachineStatus(edgeNodeID(request.Context()), report)
	}
	writeJSON(response, http.StatusAccepted, map[string]bool{"accepted": accepted})
}

func (s *Server) edgeMachineNetworkStatus(response http.ResponseWriter, request *http.Request) {
	var report domain.MachineNetworkStatus
	if !readJSON(response, request, &report) {
		return
	}
	if !domain.ValidMachineNetworkStatus(report) || report.SampleSeconds <= 0 {
		writeError(response, http.StatusBadRequest, errors.New("machine_network_status is invalid"))
		return
	}
	report.CollectedAt = report.CollectedAt.UTC()
	accepted := false
	if !report.CollectedAt.After(time.Now().UTC().Add(maxEdgeReportClockSkew)) {
		accepted = s.recordNodeMachineNetworkStatus(edgeNodeID(request.Context()), report)
	}
	writeJSON(response, http.StatusAccepted, map[string]bool{"accepted": accepted})
}

func (s *Server) edgeMachineOriginStatus(response http.ResponseWriter, request *http.Request) {
	var report domain.MachineOriginStatus
	if !readJSON(response, request, &report) {
		return
	}
	if !domain.ValidMachineOriginStatus(report) {
		writeError(response, http.StatusBadRequest, errors.New("machine_origin_status is invalid"))
		return
	}
	report.CollectedAt = report.CollectedAt.UTC()
	accepted := false
	if !report.CollectedAt.After(time.Now().UTC().Add(maxEdgeReportClockSkew)) {
		accepted = s.recordNodeMachineOriginStatus(edgeNodeID(request.Context()), report)
	}
	writeJSON(response, http.StatusAccepted, map[string]bool{"accepted": accepted})
}

func (s *Server) recordNodeMachineStatus(nodeID string, report domain.MachineStatus) bool {
	report.CollectedAt = report.CollectedAt.UTC()
	s.machineStatusMu.Lock()
	if current, found := s.machineStatuses[nodeID]; found && !report.CollectedAt.After(current.CollectedAt) {
		s.machineStatusMu.Unlock()
		return false
	}
	if s.machineStatuses == nil {
		s.machineStatuses = make(map[string]domain.MachineStatus)
	}
	s.machineStatuses[nodeID] = report
	subscribers := s.machineStatusSubscribersLocked(nodeID)
	notifyMachineStatusSubscribers(subscribers)
	s.machineStatusMu.Unlock()
	return true
}

func (s *Server) recordNodeMachineNetworkStatus(nodeID string, report domain.MachineNetworkStatus) bool {
	report.CollectedAt = report.CollectedAt.UTC()
	s.machineStatusMu.Lock()
	if current, found := s.machineNetworkStatuses[nodeID]; found && !report.CollectedAt.After(current.CollectedAt) {
		s.machineStatusMu.Unlock()
		return false
	}
	if s.machineNetworkStatuses == nil {
		s.machineNetworkStatuses = make(map[string]domain.MachineNetworkStatus)
	}
	s.machineNetworkStatuses[nodeID] = report
	subscribers := s.machineStatusSubscribersLocked(nodeID)
	notifyMachineStatusSubscribers(subscribers)
	s.machineStatusMu.Unlock()
	return true
}

func (s *Server) recordNodeMachineOriginStatus(nodeID string, report domain.MachineOriginStatus) bool {
	report.CollectedAt = report.CollectedAt.UTC()
	s.machineStatusMu.Lock()
	if current, found := s.machineOriginStatuses[nodeID]; found && !report.CollectedAt.After(current.CollectedAt) {
		s.machineStatusMu.Unlock()
		return false
	}
	if s.machineOriginStatuses == nil {
		s.machineOriginStatuses = make(map[string]domain.MachineOriginStatus)
	}
	s.machineOriginStatuses[nodeID] = report
	subscribers := s.machineStatusSubscribersLocked(nodeID)
	notifyMachineStatusSubscribers(subscribers)
	s.machineStatusMu.Unlock()
	return true
}

func (s *Server) machineStatusSubscribersLocked(nodeID string) []chan struct{} {
	subscribers := make([]chan struct{}, 0, len(s.machineStatusSubscribers[nodeID]))
	for _, subscriber := range s.machineStatusSubscribers[nodeID] {
		subscribers = append(subscribers, subscriber)
	}
	return subscribers
}

func notifyMachineStatusSubscribers(subscribers []chan struct{}) {
	// Callers hold machineStatusMu so node deletion can close subscriptions safely.
	for _, subscriber := range subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (s *Server) clearNodeMachineStatusState(nodeID string) {
	s.machineStatusMu.Lock()
	defer s.machineStatusMu.Unlock()

	if timer := s.machineStatusDemandTimers[nodeID]; timer != nil {
		timer.Stop()
	}
	for _, subscriber := range s.machineStatusSubscribers[nodeID] {
		close(subscriber)
	}
	for _, subscriber := range s.machinePolicySubscribers[nodeID] {
		close(subscriber)
	}

	delete(s.machineStatuses, nodeID)
	delete(s.machineNetworkStatuses, nodeID)
	delete(s.machineOriginStatuses, nodeID)
	delete(s.machineStatusCapabilities, nodeID)
	delete(s.machineStatusDemandActive, nodeID)
	delete(s.machineStatusDemandTimers, nodeID)
	delete(s.machineStatusDemandTimerIDs, nodeID)
	delete(s.machineStatusSubscribers, nodeID)
	delete(s.machinePolicySubscribers, nodeID)
}

func (s *Server) subscribeMachineStatus(nodeID string) (<-chan struct{}, func()) {
	s.machineStatusMu.Lock()
	if s.machineStatusSubscribers == nil {
		s.machineStatusSubscribers = make(map[string]map[uint64]chan struct{})
	}
	if s.machineStatusSubscribers[nodeID] == nil {
		s.machineStatusSubscribers[nodeID] = make(map[uint64]chan struct{})
	}
	if timer := s.machineStatusDemandTimers[nodeID]; timer != nil {
		timer.Stop()
		delete(s.machineStatusDemandTimers, nodeID)
		delete(s.machineStatusDemandTimerIDs, nodeID)
	}
	s.machineStatusSubscriberID++
	id := s.machineStatusSubscriberID
	updates := make(chan struct{}, 1)
	s.machineStatusSubscribers[nodeID][id] = updates
	activated := !s.machineStatusDemandActive[nodeID]
	if s.machineStatusDemandActive == nil {
		s.machineStatusDemandActive = make(map[string]bool)
	}
	s.machineStatusDemandActive[nodeID] = true
	if activated {
		notifyMachineStatusPolicySubscribers(s.machinePolicySubscribersLocked(nodeID), activeMachineStatusPolicy())
	}
	s.machineStatusMu.Unlock()
	return updates, func() { s.unsubscribeMachineStatus(nodeID, id) }
}

func (s *Server) unsubscribeMachineStatus(nodeID string, id uint64) {
	s.machineStatusMu.Lock()
	subscribers, found := s.machineStatusSubscribers[nodeID]
	if !found {
		s.machineStatusMu.Unlock()
		return
	}
	if _, found := subscribers[id]; !found {
		s.machineStatusMu.Unlock()
		return
	}
	delete(subscribers, id)
	if len(subscribers) != 0 {
		s.machineStatusMu.Unlock()
		return
	}
	delete(s.machineStatusSubscribers, nodeID)
	if s.machineStatusDemandTimers == nil {
		s.machineStatusDemandTimers = make(map[string]*time.Timer)
	}
	if s.machineStatusDemandTimerIDs == nil {
		s.machineStatusDemandTimerIDs = make(map[string]uint64)
	}
	if current := s.machineStatusDemandTimers[nodeID]; current != nil {
		current.Stop()
	}
	grace := s.machineStatusDemandGrace
	if grace <= 0 {
		grace = defaultMachineStatusDemandGrace
	}
	s.machineStatusDemandTimerID++
	timerID := s.machineStatusDemandTimerID
	timer := time.AfterFunc(grace, func() { s.expireMachineStatusDemand(nodeID, timerID) })
	s.machineStatusDemandTimers[nodeID] = timer
	s.machineStatusDemandTimerIDs[nodeID] = timerID
	s.machineStatusMu.Unlock()
}

func (s *Server) expireMachineStatusDemand(nodeID string, timerID uint64) {
	s.machineStatusMu.Lock()
	if s.machineStatusDemandTimerIDs[nodeID] != timerID || len(s.machineStatusSubscribers[nodeID]) != 0 {
		s.machineStatusMu.Unlock()
		return
	}
	delete(s.machineStatusDemandTimers, nodeID)
	delete(s.machineStatusDemandTimerIDs, nodeID)
	if !s.machineStatusDemandActive[nodeID] {
		s.machineStatusMu.Unlock()
		return
	}
	delete(s.machineStatusDemandActive, nodeID)
	policySubscribers := s.machinePolicySubscribersLocked(nodeID)
	notifyMachineStatusPolicySubscribers(policySubscribers, inactiveMachineStatusPolicy())
	s.machineStatusMu.Unlock()
}

func activeMachineStatusPolicy() domain.MachineStatusSamplingPolicy {
	return domain.MachineStatusSamplingPolicy{
		HostIntervalSeconds:    domain.ActiveMachineStatusIntervalSeconds,
		NetworkIntervalSeconds: domain.DefaultMachineNetworkIntervalSeconds,
		OriginIntervalSeconds:  domain.DefaultMachineOriginIntervalSeconds,
	}
}

func inactiveMachineStatusPolicy() domain.MachineStatusSamplingPolicy {
	return domain.MachineStatusSamplingPolicy{
		HostIntervalSeconds:   domain.DefaultMachineStatusIntervalSeconds,
		OriginIntervalSeconds: domain.DefaultMachineOriginIntervalSeconds,
	}
}

func (s *Server) machinePolicySubscribersLocked(nodeID string) []chan domain.MachineStatusSamplingPolicy {
	subscribers := make([]chan domain.MachineStatusSamplingPolicy, 0, len(s.machinePolicySubscribers[nodeID]))
	for _, subscriber := range s.machinePolicySubscribers[nodeID] {
		subscribers = append(subscribers, subscriber)
	}
	return subscribers
}

func notifyMachineStatusPolicySubscribers(subscribers []chan domain.MachineStatusSamplingPolicy, policy domain.MachineStatusSamplingPolicy) {
	// Callers hold machineStatusMu so node deletion can close subscriptions safely.
	for _, subscriber := range subscribers {
		select {
		case subscriber <- policy:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- policy:
			default:
			}
		}
	}
}

func (s *Server) subscribeMachineStatusPolicy(nodeID string) (<-chan domain.MachineStatusSamplingPolicy, domain.MachineStatusSamplingPolicy, func()) {
	s.machineStatusMu.Lock()
	defer s.machineStatusMu.Unlock()
	if s.machinePolicySubscribers == nil {
		s.machinePolicySubscribers = make(map[string]map[uint64]chan domain.MachineStatusSamplingPolicy)
	}
	if s.machinePolicySubscribers[nodeID] == nil {
		s.machinePolicySubscribers[nodeID] = make(map[uint64]chan domain.MachineStatusSamplingPolicy)
	}
	s.machinePolicySubscriberID++
	id := s.machinePolicySubscriberID
	updates := make(chan domain.MachineStatusSamplingPolicy, 1)
	s.machinePolicySubscribers[nodeID][id] = updates
	policy := inactiveMachineStatusPolicy()
	if s.machineStatusDemandActive[nodeID] {
		policy = activeMachineStatusPolicy()
	}
	return updates, policy, func() {
		s.machineStatusMu.Lock()
		defer s.machineStatusMu.Unlock()
		subscribers, found := s.machinePolicySubscribers[nodeID]
		if !found {
			return
		}
		if _, found := subscribers[id]; !found {
			return
		}
		delete(subscribers, id)
		if len(subscribers) == 0 {
			delete(s.machinePolicySubscribers, nodeID)
		}
	}
}

func (s *Server) machineStatusEvents(response http.ResponseWriter, request *http.Request) {
	node, err := s.Store.GetNode(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	capabilities := s.seedMachineStatusCapabilityProfile(node.ID, node.Capabilities)
	updates, unsubscribe := s.subscribeMachineStatus(node.ID)
	defer unsubscribe()
	capabilities = s.currentMachineStatusCapabilityProfile(node.ID, capabilities)

	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(response, "retry: 3000\n\n"); err != nil {
		return
	}
	if !writeMachineStatusEvent(response, s.nodeMachineStatusWithCapabilityProfile(node.ID, capabilities, time.Now().UTC())) {
		return
	}
	controller := http.NewResponseController(response)
	if err := controller.Flush(); err != nil {
		return
	}

	keepAlive := time.NewTicker(machineStatusSSEKeepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case _, ok := <-updates:
			if !ok {
				return
			}
			capabilities = s.currentMachineStatusCapabilityProfile(node.ID, capabilities)
			status := s.nodeMachineStatusWithCapabilityProfile(node.ID, capabilities, time.Now().UTC())
			if !writeMachineStatusEvent(response, status) {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		case <-keepAlive.C:
			if !s.machineStatusSessionActive(request) {
				return
			}
			if _, err := fmt.Fprint(response, ": keepalive\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *Server) machineStatusPolicyEvents(response http.ResponseWriter, request *http.Request) {
	updates, initial, unsubscribe := s.subscribeMachineStatusPolicy(edgeNodeID(request.Context()))
	defer unsubscribe()

	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(response, "retry: 3000\n\n"); err != nil {
		return
	}
	if !writeMachineStatusPolicyEvent(response, initial) {
		return
	}
	controller := http.NewResponseController(response)
	if err := controller.Flush(); err != nil {
		return
	}

	keepAlive := time.NewTicker(machineStatusSSEKeepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case policy, ok := <-updates:
			if !ok {
				return
			}
			if !writeMachineStatusPolicyEvent(response, policy) {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		case <-keepAlive.C:
			if _, err := fmt.Fprint(response, ": keepalive\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

func writeMachineStatusEvent(response http.ResponseWriter, status nodeMachineStatusResponse) bool {
	payload, err := json.Marshal(status)
	if err != nil {
		return false
	}
	id := time.Now().UTC().UnixMilli()
	latest := time.Time{}
	if status.Report != nil {
		latest = status.Report.CollectedAt
	}
	if status.Network != nil && status.Network.CollectedAt.After(latest) {
		latest = status.Network.CollectedAt
	}
	if status.OriginCollectedAt != nil && status.OriginCollectedAt.After(latest) {
		latest = *status.OriginCollectedAt
	}
	if !latest.IsZero() {
		id = latest.UTC().UnixMilli()
	}
	_, err = fmt.Fprintf(response, "id: %d\nevent: machine-status\ndata: %s\n\n", id, payload)
	return err == nil
}

func writeMachineStatusPolicyEvent(response http.ResponseWriter, policy domain.MachineStatusSamplingPolicy) bool {
	payload, err := json.Marshal(policy)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(response, "event: machine-status-policy\ndata: %s\n\n", payload)
	return err == nil
}

func (s *Server) machineStatusSessionActive(request *http.Request) bool {
	cookie, err := request.Cookie("cdn_session")
	if err != nil {
		return false
	}
	_, err = s.Store.Session(cookie.Value)
	return err == nil
}
