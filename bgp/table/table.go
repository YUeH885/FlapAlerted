package table

import (
	"FlapAlerted/bgp/common"
	"FlapAlerted/bgp/notification"
	"FlapAlerted/config"
	"context"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const recentPrefixWindowSec int64 = 300

type PrefixTable struct {
	table               map[netip.Prefix]*Entry
	pathChangeChan      chan PathChange
	importCount         atomic.Uint32
	pathChangeCount     atomic.Uint64
	peerPathChangeCount map[uint32]uint64
	prefixPathChanges   map[netip.Prefix][]prefixPathChange
	pathChangeStatsMux  sync.RWMutex
	sessionCancellation context.CancelCauseFunc
}

func NewPrefixTable(pathChangeChan chan PathChange, sessionCancellation context.CancelCauseFunc) *PrefixTable {
	return &PrefixTable{
		table:               make(map[netip.Prefix]*Entry),
		pathChangeChan:      pathChangeChan,
		peerPathChangeCount: make(map[uint32]uint64),
		prefixPathChanges:   make(map[netip.Prefix][]prefixPathChange),
		sessionCancellation: sessionCancellation,
	}
}

type PathChange struct {
	Prefix       netip.Prefix
	IsWithdrawal bool
	OldPath      common.AsPath
}

type RecentPrefixChange struct {
	Prefix       netip.Prefix
	RouteChanges int
	RateSec      float64
}

type PrefixReport struct {
	Prefix           netip.Prefix
	PathHistory      []common.PathInfo
	TotalPathChanges uint64
	RateSecHistory   []int
	FirstSeen        int64
}

type prefixPathChange struct {
	timestamp    int64
	path         common.AsPath
	isWithdrawal bool
}

type Entry struct {
	Paths map[uint32]common.AsPath
}

func (t *PrefixTable) update(prefix netip.Prefix, pathID uint32, isWithdrawal bool, asPath common.AsPath) {
	if isWithdrawal {
		if entry, ok := t.table[prefix]; ok {
			if oldPath, exists := entry.Paths[pathID]; exists {
				t.pathChangeChan <- PathChange{
					Prefix:       prefix,
					IsWithdrawal: true,
					OldPath:      oldPath,
				}
				t.pathChangeCount.Add(1)
				t.addPathChangeStats(prefix, oldPath, true)
				t.importCount.Add(^uint32(0))
				delete(entry.Paths, pathID)
				if len(entry.Paths) == 0 {
					delete(t.table, prefix)
				}
			}
		}
	} else {
		entry, found := t.table[prefix]
		if !found {
			t.importCount.Add(1)
			entry = &Entry{Paths: make(map[uint32]common.AsPath)}
			t.table[prefix] = entry
		} else {
			if oldPath, existed := entry.Paths[pathID]; existed {
				t.pathChangeChan <- PathChange{
					Prefix:       prefix,
					IsWithdrawal: false,
					OldPath:      oldPath,
				}
				t.pathChangeCount.Add(1)
				t.addPathChangeStats(prefix, oldPath, false)
			} else {
				t.importCount.Add(1)
			}
		}
		entry.Paths[pathID] = asPath
		if t.importCount.Load() > config.GlobalConf.ImportLimit {
			t.sessionCancellation(notification.ErrImportLimit)
		}
	}
}

func (t *PrefixTable) ImportCount() uint32 {
	return t.importCount.Load()
}

func (t *PrefixTable) PathChangeCount() uint64 {
	return t.pathChangeCount.Load()
}

func (t *PrefixTable) addPathChangeStats(prefix netip.Prefix, oldPath common.AsPath, isWithdrawal bool) {
	now := time.Now().Unix()
	cutoff := now - recentPrefixWindowSec

	t.pathChangeStatsMux.Lock()
	defer t.pathChangeStatsMux.Unlock()

	if len(oldPath) != 0 {
		t.peerPathChangeCount[oldPath[0]]++
	}

	history := t.prunePrefixChangesLocked(prefix, cutoff)
	t.prefixPathChanges[prefix] = append(history, prefixPathChange{
		timestamp:    now,
		path:         append(common.AsPath(nil), oldPath...),
		isWithdrawal: isWithdrawal,
	})
}

func (t *PrefixTable) PathChangeCountsByPeerASN() map[uint32]uint64 {
	t.pathChangeStatsMux.RLock()
	defer t.pathChangeStatsMux.RUnlock()
	return maps.Clone(t.peerPathChangeCount)
}

func (t *PrefixTable) RecentPrefixChanges() []RecentPrefixChange {
	cutoff := time.Now().Unix() - recentPrefixWindowSec
	t.pathChangeStatsMux.Lock()
	defer t.pathChangeStatsMux.Unlock()

	changes := make([]RecentPrefixChange, 0, len(t.prefixPathChanges))
	for prefix := range t.prefixPathChanges {
		history := t.prunePrefixChangesLocked(prefix, cutoff)
		if len(history) == 0 {
			continue
		}
		changes = append(changes, RecentPrefixChange{
			Prefix:       prefix,
			RouteChanges: len(history),
			RateSec:      float64(len(history)) / float64(recentPrefixWindowSec),
		})
	}
	return changes
}

func (t *PrefixTable) PruneRecentPrefixChanges() {
	cutoff := time.Now().Unix() - recentPrefixWindowSec
	t.pathChangeStatsMux.Lock()
	defer t.pathChangeStatsMux.Unlock()

	for prefix := range t.prefixPathChanges {
		t.prunePrefixChangesLocked(prefix, cutoff)
	}
}

func (t *PrefixTable) PrefixReport(prefix netip.Prefix) (PrefixReport, bool) {
	now := time.Now().Unix()
	cutoff := now - recentPrefixWindowSec

	t.pathChangeStatsMux.Lock()
	defer t.pathChangeStatsMux.Unlock()

	changes := t.prunePrefixChangesLocked(prefix, cutoff)
	if len(changes) == 0 {
		return PrefixReport{}, false
	}

	paths := make(map[string]common.PathInfo)
	rateSecHistory := make([]int, 6)
	for _, change := range changes {
		key := common.PathKey(change.path)
		path, ok := paths[key]
		if !ok {
			path.Path = append(common.AsPath(nil), change.path...)
		}
		if change.isWithdrawal {
			path.WithdrawalCount++
		} else {
			path.AnnouncementCount++
		}
		paths[key] = path
		bucket := int((change.timestamp - cutoff) / 60)
		bucket = min(max(bucket, 0), len(rateSecHistory)-2)
		rateSecHistory[bucket+1]++
	}
	for i := range rateSecHistory {
		rateSecHistory[i] /= 60
	}

	return PrefixReport{
		Prefix:           prefix,
		PathHistory:      slices.Collect(maps.Values(paths)),
		TotalPathChanges: uint64(len(changes)),
		RateSecHistory:   rateSecHistory,
		FirstSeen:        changes[0].timestamp,
	}, true
}

func (t *PrefixTable) prunePrefixChangesLocked(prefix netip.Prefix, cutoff int64) []prefixPathChange {
	history := t.prefixPathChanges[prefix]
	i := 0
	for i < len(history) && history[i].timestamp < cutoff {
		i++
	}
	history = history[i:]
	if len(history) == 0 {
		delete(t.prefixPathChanges, prefix)
		return nil
	}
	t.prefixPathChanges[prefix] = history
	return history
}
