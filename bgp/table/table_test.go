package table

import (
	"FlapAlerted/bgp/common"
	"net/netip"
	"testing"
	"time"
)

func TestPathChangeCountTracksEmittedChanges(t *testing.T) {
	pathChanges := make(chan PathChange, 2)
	table := NewPrefixTable(pathChanges, func(error) {})
	prefix := netip.MustParsePrefix("192.0.2.0/24")

	table.update(prefix, 0, false, common.AsPath{65000})
	if got := table.PathChangeCount(); got != 0 {
		t.Fatalf("initial import counted as path change: got %d", got)
	}

	table.update(prefix, 0, false, common.AsPath{65001})
	table.update(prefix, 0, true, nil)

	if got := table.PathChangeCount(); got != 2 {
		t.Fatalf("path change count = %d, want 2", got)
	}

	peerCounts := table.PathChangeCountsByPeerASN()
	if got := peerCounts[65000]; got != 1 {
		t.Fatalf("peer 65000 path change count = %d, want 1", got)
	}
	if got := peerCounts[65001]; got != 1 {
		t.Fatalf("peer 65001 path change count = %d, want 1", got)
	}

	recent := table.RecentPrefixChanges()
	if len(recent) != 1 {
		t.Fatalf("recent prefix changes length = %d, want 1", len(recent))
	}
	if recent[0].Prefix != prefix {
		t.Fatalf("recent prefix = %s, want %s", recent[0].Prefix, prefix)
	}
	if got := recent[0].RouteChanges; got != 2 {
		t.Fatalf("recent prefix route changes = %d, want 2", got)
	}
	if got, want := recent[0].RateSec, float64(2)/float64(recentPrefixWindowSec); got != want {
		t.Fatalf("recent prefix rate = %f, want %f", got, want)
	}

	report, found := table.PrefixReport(prefix)
	if !found {
		t.Fatal("prefix report not found")
	}
	if got := report.TotalPathChanges; got != 2 {
		t.Fatalf("prefix report total path changes = %d, want 2", got)
	}
	if len(report.PathHistory) != 2 {
		t.Fatalf("prefix report path history length = %d, want 2", len(report.PathHistory))
	}
}

func TestPruneRecentPrefixChangesRemovesExpiredEntries(t *testing.T) {
	table := NewPrefixTable(make(chan PathChange, 1), func(error) {})
	oldPrefix := netip.MustParsePrefix("192.0.2.0/24")
	recentPrefix := netip.MustParsePrefix("198.51.100.0/24")
	now := time.Now().Unix()

	table.prefixPathChanges[oldPrefix] = []prefixPathChange{{
		timestamp: now - recentPrefixWindowSec - 1,
		path:      common.AsPath{65000},
	}}
	table.prefixPathChanges[recentPrefix] = []prefixPathChange{{
		timestamp: now,
		path:      common.AsPath{65001},
	}}

	table.PruneRecentPrefixChanges()

	if _, ok := table.prefixPathChanges[oldPrefix]; ok {
		t.Fatal("expired prefix change was not pruned")
	}
	if _, ok := table.prefixPathChanges[recentPrefix]; !ok {
		t.Fatal("recent prefix change was pruned")
	}
}
