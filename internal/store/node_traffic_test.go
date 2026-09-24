package store

import (
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestNodeTrafficMonthsSurviveRestartAndHandleCounterDiscontinuities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("traffic-edge", "203.0.113.41")
	if err != nil {
		t.Fatal(err)
	}
	record := func(at time.Time, iface, boot string, rx, tx int64, want bool) {
		t.Helper()
		accepted, err := database.RecordNodeTrafficSample(node.ID, iface,
			domain.MachineNetworkCounters{BootID: boot, RXBytes: rx, TXBytes: tx}, at)
		if err != nil || accepted != want {
			t.Fatalf("record node traffic at %s: accepted=%v, err=%v", at, accepted, err)
		}
	}
	august := time.Date(2026, 8, 31, 23, 59, 0, 0, time.UTC)
	record(august, "eth0", "boot-1", 100, 200, true)
	record(august.Add(2*time.Minute), "eth0", "boot-1", 220, 320, true)
	record(august.Add(2*time.Minute), "eth0", "boot-1", 220, 320, false)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	record(august.Add(3*time.Minute), "eth0", "boot-1", 250, 350, true)

	months, err := database.nodeTrafficMonthsAt(node.ID, 12, august.Add(10*time.Minute))
	if err != nil || len(months) != 2 {
		t.Fatalf("traffic months = %#v, err=%v", months, err)
	}
	if months[0].Month != "2026-09" || months[0].RXBytes != 90 || months[0].TXBytes != 90 || months[0].Partial || !months[0].Estimated {
		t.Fatalf("September traffic = %#v", months[0])
	}
	if months[1].Month != "2026-08" || months[1].RXBytes != 60 || months[1].TXBytes != 60 || !months[1].Partial || !months[1].Estimated {
		t.Fatalf("August traffic = %#v", months[1])
	}
	closed, err := database.nodeTrafficMonthsAt(node.ID, 12, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || !closed[0].Partial {
		t.Fatalf("month without a closing sample was treated as complete: %#v, err=%v", closed, err)
	}

	record(august.Add(4*time.Minute), "eth0", "boot-2", 20, 30, true)
	record(august.Add(5*time.Minute), "ens3", "boot-2", 50, 50, true)
	record(august.Add(6*time.Minute), "ens3", "boot-2", 70, 80, true)
	current, err := database.CurrentNodeTraffic(node.ID, august.Add(6*time.Minute))
	if err != nil || current == nil || current.RXBytes != 110 || current.TXBytes != 120 || !current.Partial {
		t.Fatalf("traffic after reboot and interface switch = %#v, err=%v", current, err)
	}
	if err := database.DeleteNode(node.ID); err != nil {
		t.Fatal(err)
	}
	months, err = database.nodeTrafficMonthsAt(node.ID, 12, august.Add(10*time.Minute))
	if err != nil || len(months) != 0 {
		t.Fatalf("deleted node traffic = %#v, err=%v", months, err)
	}
}

func TestNodeTrafficMonthKeepsLaterSubsecondSample(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("subsecond-traffic-edge", "203.0.113.42")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		at time.Time
		rx int64
	}{
		{at: first, rx: 100},
		{at: first.Add(500 * time.Millisecond), rx: 150},
	} {
		if _, err := database.RecordNodeTrafficSample(node.ID, "eth0", domain.MachineNetworkCounters{
			BootID: "boot-1", RXBytes: sample.rx, TXBytes: 0,
		}, sample.at); err != nil {
			t.Fatal(err)
		}
	}
	current, err := database.CurrentNodeTraffic(node.ID, first)
	if err != nil || current == nil || !current.CollectedAt.Equal(first.Add(500*time.Millisecond)) || current.RXBytes != 50 {
		t.Fatalf("subsecond monthly traffic = %#v, err=%v", current, err)
	}
}
