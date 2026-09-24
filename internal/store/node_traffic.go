package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"simple_cdn/internal/domain"
)

const maxNodeTrafficSampleGap = 45 * 24 * time.Hour

type NodeTrafficMonth struct {
	Month       string    `json:"month"`
	RXBytes     int64     `json:"rx_bytes"`
	TXBytes     int64     `json:"tx_bytes"`
	Partial     bool      `json:"partial"`
	Estimated   bool      `json:"estimated"`
	CollectedAt time.Time `json:"collected_at"`
}

type nodeTrafficSample struct {
	bootID           string
	networkInterface string
	rxBytes          int64
	txBytes          int64
	collectedAt      time.Time
}

func trafficMonth(at time.Time) string { return at.UTC().Format("2006-01") }

func (s *Store) RecordNodeTrafficSample(nodeID, networkInterface string, counters domain.MachineNetworkCounters, collectedAt time.Time) (bool, error) {
	collectedAt = collectedAt.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var previous nodeTrafficSample
	var previousAt string
	err = tx.QueryRow(`SELECT boot_id, network_interface, rx_bytes, tx_bytes, collected_at
		FROM node_traffic_samples WHERE node_id = ?`, nodeID).Scan(
		&previous.bootID, &previous.networkInterface, &previous.rxBytes, &previous.txBytes, &previousAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	first := errors.Is(err, sql.ErrNoRows)
	if !first {
		previous.collectedAt, err = parseTime(previousAt)
		if err != nil {
			return false, fmt.Errorf("parse previous node traffic sample: %w", err)
		}
		if !collectedAt.After(previous.collectedAt) {
			return false, nil
		}
	}

	currentMonth := trafficMonth(collectedAt)
	continuous := !first && counters.BootID == previous.bootID && networkInterface == previous.networkInterface &&
		counters.RXBytes >= previous.rxBytes && counters.TXBytes >= previous.txBytes &&
		collectedAt.Sub(previous.collectedAt) <= maxNodeTrafficSampleGap
	if !continuous {
		if !first && trafficMonth(previous.collectedAt) != currentMonth {
			if err := upsertNodeTrafficMonth(tx, nodeID, trafficMonth(previous.collectedAt), 0, 0, true, false, previous.collectedAt); err != nil {
				return false, err
			}
		}
		if err := upsertNodeTrafficMonth(tx, nodeID, currentMonth, 0, 0, true, false, collectedAt); err != nil {
			return false, err
		}
	} else {
		rxRemaining := counters.RXBytes - previous.rxBytes
		txRemaining := counters.TXBytes - previous.txBytes
		crossesMonth := trafficMonth(previous.collectedAt) != currentMonth
		for segmentStart := previous.collectedAt; segmentStart.Before(collectedAt); {
			nextMonth := time.Date(segmentStart.Year(), segmentStart.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			segmentEnd := collectedAt
			if nextMonth.Before(segmentEnd) {
				segmentEnd = nextMonth
			}
			rxBytes, txBytes := rxRemaining, txRemaining
			if segmentEnd.Before(collectedAt) {
				fraction := float64(segmentEnd.Sub(segmentStart)) / float64(collectedAt.Sub(segmentStart))
				rxBytes = int64(math.Round(float64(rxRemaining) * fraction))
				txBytes = int64(math.Round(float64(txRemaining) * fraction))
			}
			if err := upsertNodeTrafficMonth(tx, nodeID, trafficMonth(segmentStart), rxBytes, txBytes, false, crossesMonth, segmentEnd); err != nil {
				return false, err
			}
			rxRemaining -= rxBytes
			txRemaining -= txBytes
			segmentStart = segmentEnd
		}
	}

	_, err = tx.Exec(`INSERT INTO node_traffic_samples
		(node_id, boot_id, network_interface, rx_bytes, tx_bytes, collected_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET boot_id = excluded.boot_id,
			network_interface = excluded.network_interface, rx_bytes = excluded.rx_bytes,
			tx_bytes = excluded.tx_bytes, collected_at = excluded.collected_at`,
		nodeID, counters.BootID, networkInterface, counters.RXBytes, counters.TXBytes, stamp(collectedAt))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func upsertNodeTrafficMonth(tx *sql.Tx, nodeID, month string, rxBytes, txBytes int64, partial, estimated bool, collectedAt time.Time) error {
	_, err := tx.Exec(`INSERT INTO node_monthly_traffic
		(node_id, month, rx_bytes, tx_bytes, partial, estimated, collected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, month) DO UPDATE SET
			rx_bytes = node_monthly_traffic.rx_bytes + excluded.rx_bytes,
			tx_bytes = node_monthly_traffic.tx_bytes + excluded.tx_bytes,
			partial = node_monthly_traffic.partial OR excluded.partial,
			estimated = node_monthly_traffic.estimated OR excluded.estimated,
			collected_at = excluded.collected_at`,
		nodeID, month, rxBytes, txBytes, partial, estimated, stamp(collectedAt))
	return err
}

func (s *Store) NodeTrafficMonths(nodeID string, limit int) ([]NodeTrafficMonth, error) {
	return s.nodeTrafficMonthsAt(nodeID, limit, time.Now().UTC())
}

func (s *Store) nodeTrafficMonthsAt(nodeID string, limit int, at time.Time) ([]NodeTrafficMonth, error) {
	if limit < 1 || limit > 24 {
		return nil, fmt.Errorf("invalid node traffic month limit: %d", limit)
	}
	rows, err := s.db.Query(`SELECT month, rx_bytes, tx_bytes, partial, estimated, collected_at
		FROM node_monthly_traffic WHERE node_id = ? ORDER BY month DESC LIMIT ?`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	months := make([]NodeTrafficMonth, 0)
	for rows.Next() {
		var month NodeTrafficMonth
		var collectedAtStamp string
		if err := rows.Scan(&month.Month, &month.RXBytes, &month.TXBytes, &month.Partial, &month.Estimated, &collectedAtStamp); err != nil {
			return nil, err
		}
		month.CollectedAt, err = parseTime(collectedAtStamp)
		if err != nil {
			return nil, err
		}
		monthStart, err := time.Parse("2006-01", month.Month)
		if err != nil {
			return nil, err
		}
		monthEnd := monthStart.AddDate(0, 1, 0)
		if !at.Before(monthEnd) && month.CollectedAt.Before(monthEnd) {
			month.Partial = true
		}
		months = append(months, month)
	}
	return months, rows.Err()
}

func (s *Store) CurrentNodeTraffic(nodeID string, at time.Time) (*NodeTrafficMonth, error) {
	var month NodeTrafficMonth
	var collectedAt string
	err := s.db.QueryRow(`SELECT month, rx_bytes, tx_bytes, partial, estimated, collected_at
		FROM node_monthly_traffic WHERE node_id = ? AND month = ?`, nodeID, trafficMonth(at)).Scan(
		&month.Month, &month.RXBytes, &month.TXBytes, &month.Partial, &month.Estimated, &collectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	month.CollectedAt, err = parseTime(collectedAt)
	return &month, err
}
