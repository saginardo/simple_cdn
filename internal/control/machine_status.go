package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"simple_cdn/internal/domain"
)

const machineStatusSSEKeepAlive = 15 * time.Second

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
	subscribers := make([]chan domain.MachineStatus, 0, len(s.machineStatusSubscribers[nodeID]))
	for _, subscriber := range s.machineStatusSubscribers[nodeID] {
		subscribers = append(subscribers, subscriber)
	}
	s.machineStatusMu.Unlock()

	for _, subscriber := range subscribers {
		select {
		case subscriber <- report:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- report:
			default:
			}
		}
	}
	return true
}

func (s *Server) siteOriginCircuitUnavailable(nodeID, siteID string) bool {
	s.machineStatusMu.RLock()
	defer s.machineStatusMu.RUnlock()
	report, found := s.machineStatuses[nodeID]
	if !found {
		return false
	}
	for _, probe := range report.OriginProbes {
		if probe.CircuitState == domain.OriginCircuitClosed {
			continue
		}
		for _, reference := range probe.References {
			if reference.SiteID == siteID && reference.Role == "primary" {
				return true
			}
		}
	}
	return false
}

func (s *Server) subscribeMachineStatus(nodeID string) (<-chan domain.MachineStatus, func()) {
	s.machineStatusMu.Lock()
	defer s.machineStatusMu.Unlock()
	if s.machineStatusSubscribers == nil {
		s.machineStatusSubscribers = make(map[string]map[uint64]chan domain.MachineStatus)
	}
	if s.machineStatusSubscribers[nodeID] == nil {
		s.machineStatusSubscribers[nodeID] = make(map[uint64]chan domain.MachineStatus)
	}
	s.machineStatusSubscriberID++
	id := s.machineStatusSubscriberID
	updates := make(chan domain.MachineStatus, 1)
	s.machineStatusSubscribers[nodeID][id] = updates
	return updates, func() {
		s.machineStatusMu.Lock()
		defer s.machineStatusMu.Unlock()
		delete(s.machineStatusSubscribers[nodeID], id)
		if len(s.machineStatusSubscribers[nodeID]) == 0 {
			delete(s.machineStatusSubscribers, nodeID)
		}
	}
}

func (s *Server) machineStatusEvents(response http.ResponseWriter, request *http.Request) {
	node, err := s.Store.GetNode(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	updates, unsubscribe := s.subscribeMachineStatus(node.ID)
	defer unsubscribe()

	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(response, "retry: 3000\n\n"); err != nil {
		return
	}
	if !writeMachineStatusEvent(response, s.nodeMachineStatus(node, time.Now().UTC())) {
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
		case <-updates:
			status := s.nodeMachineStatus(node, time.Now().UTC())
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

func writeMachineStatusEvent(response http.ResponseWriter, status nodeMachineStatusResponse) bool {
	payload, err := json.Marshal(status)
	if err != nil {
		return false
	}
	id := time.Now().UTC().UnixMilli()
	if status.Report != nil {
		id = status.Report.CollectedAt.UTC().UnixMilli()
	}
	_, err = fmt.Fprintf(response, "id: %d\nevent: machine-status\ndata: %s\n\n", id, payload)
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
