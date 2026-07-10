package session

import (
	"FlapAlerted/bgp/common"
	"FlapAlerted/bgp/table"
	"cmp"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// Established session tracker
var (
	sessionTracker     = make(map[net.Conn]establishedSession)
	sessionTrackerLock sync.RWMutex
)

const maxRateHistory = 60

type establishedSession struct {
	Remote        string
	EstablishTime int64
	session       *common.LocalSession
	table         *table.PrefixTable

	rateSecHistory      []int
	lastPathChangeCount uint64
	rateSec             int

	peerRateSecHistory      map[uint32][]int
	lastPeerPathChangeCount map[uint32]uint64
}

type PeerRate struct {
	ASN        uint32
	RateSecAvg float64
	RateSec    int
}

type Info struct {
	Remote         string
	RouterID       string
	Hostname       string
	EstablishTime  int64
	ImportCount    uint32
	RateSecAvg     float64
	RateSec        int
	PeerRates      []PeerRate
	RecentPrefixes []table.RecentPrefixChange
}

func AddSession(conn net.Conn, session *common.LocalSession, table *table.PrefixTable) {
	newSession := establishedSession{
		Remote:                  conn.RemoteAddr().String(),
		EstablishTime:           time.Now().Unix(),
		session:                 session,
		table:                   table,
		rateSec:                 -1,
		lastPathChangeCount:     table.PathChangeCount(),
		peerRateSecHistory:      make(map[uint32][]int),
		lastPeerPathChangeCount: make(map[uint32]uint64),
	}
	sessionTrackerLock.Lock()
	defer sessionTrackerLock.Unlock()
	sessionTracker[conn] = newSession
}

func RemoveSession(conn net.Conn) {
	sessionTrackerLock.Lock()
	defer sessionTrackerLock.Unlock()
	delete(sessionTracker, conn)
}

func GetSessionCount() int {
	sessionTrackerLock.RLock()
	defer sessionTrackerLock.RUnlock()
	return len(sessionTracker)
}

func UpdateRates(intervalSec uint64) {
	sessionTrackerLock.Lock()
	defer sessionTrackerLock.Unlock()
	for conn, session := range sessionTracker {
		count := session.table.PathChangeCount()
		session.rateSec = int((count - session.lastPathChangeCount) / intervalSec)
		session.lastPathChangeCount = count
		session.rateSecHistory = append(session.rateSecHistory, session.rateSec)
		if len(session.rateSecHistory) > maxRateHistory {
			session.rateSecHistory = session.rateSecHistory[1:]
		}
		session.updatePeerRates(intervalSec)
		session.table.PruneRecentPrefixChanges()
		sessionTracker[conn] = session
	}
}

func GetTotalImportCount() uint32 {
	sessionTrackerLock.RLock()
	defer sessionTrackerLock.RUnlock()
	var totalCount uint32
	for _, session := range sessionTracker {
		totalCount += session.table.ImportCount()
	}
	return totalCount
}

func rateSecAvg(history []int) float64 {
	if len(history) == 0 {
		return -1
	}
	sum := 0
	for _, rate := range history {
		sum += rate
	}
	return float64(sum) / float64(maxRateHistory)
}

func (s *establishedSession) updatePeerRates(intervalSec uint64) {
	counts := s.table.PathChangeCountsByPeerASN()
	for asn, count := range counts {
		last := s.lastPeerPathChangeCount[asn]
		rate := int((count - last) / intervalSec)
		s.lastPeerPathChangeCount[asn] = count
		history := s.peerRateSecHistory[asn]
		if rate == 0 && len(history) == 0 {
			continue
		}

		history = append(history, rate)
		if len(history) > maxRateHistory {
			history = history[1:]
		}
		if len(history) == maxRateHistory && rateSecAvg(history) == 0 {
			delete(s.peerRateSecHistory, asn)
			continue
		}
		s.peerRateSecHistory[asn] = history
	}
}

func (s establishedSession) peerRates() []PeerRate {
	rates := make([]PeerRate, 0, len(s.peerRateSecHistory))
	for asn, history := range s.peerRateSecHistory {
		if len(history) == 0 {
			continue
		}
		rates = append(rates, PeerRate{
			ASN:        asn,
			RateSec:    history[len(history)-1],
			RateSecAvg: rateSecAvg(history),
		})
	}
	slices.SortFunc(rates, func(a, b PeerRate) int {
		return cmp.Compare(b.RateSecAvg, a.RateSecAvg)
	})
	return rates
}

func GetSessionInfo(includePeerRates bool) []Info {
	sessionTrackerLock.RLock()
	defer sessionTrackerLock.RUnlock()
	var sessions = make([]Info, 0, len(sessionTracker))
	for _, session := range sessionTracker {
		info := Info{
			Remote:        session.Remote,
			RouterID:      session.session.RemoteRouterID.String(),
			Hostname:      session.session.RemoteHostname,
			EstablishTime: session.EstablishTime,
			ImportCount:   session.table.ImportCount(),
			RateSecAvg:    rateSecAvg(session.rateSecHistory),
			RateSec:       session.rateSec,
		}
		if includePeerRates {
			info.PeerRates = session.peerRates()
			info.RecentPrefixes = session.table.RecentPrefixChanges()
			slices.SortFunc(info.RecentPrefixes, func(a, b table.RecentPrefixChange) int {
				if n := cmp.Compare(b.RouteChanges, a.RouteChanges); n != 0 {
					return n
				}
				return cmp.Compare(a.Prefix.String(), b.Prefix.String())
			})
		}
		sessions = append(sessions, info)
	}
	return sessions
}

func GetSessionPrefixReport(remote string, prefix netip.Prefix) (table.PrefixReport, bool) {
	sessionTrackerLock.RLock()
	defer sessionTrackerLock.RUnlock()
	for _, session := range sessionTracker {
		if session.Remote != remote {
			continue
		}
		return session.table.PrefixReport(prefix)
	}
	return table.PrefixReport{}, false
}
