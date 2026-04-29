package quality

import (
	"sync"
	"time"
)

type PeerReport struct {
	PeerID              string
	Room                string
	Quality             string
	RoundTripTimeMs     float64
	PacketLossPercent   float64
	OutboundBitrateKbps int
	InboundBitrateKbps  int
	CandidateType       string
	JitterMs            float64
	UpdatedAt           time.Time
}

type Aggregate struct {
	PeersGood   int     `json:"peers_good"`
	PeersMedium int     `json:"peers_medium"`
	PeersPoor   int     `json:"peers_poor"`
	AvgRttMs    float64 `json:"avg_rtt_ms"`
	AvgLossPct  float64 `json:"avg_loss_pct"`
	RelayCount  int     `json:"relay_count"`
}

type Store struct {
	mu      sync.RWMutex
	reports map[string]PeerReport
}

var Default = &Store{reports: make(map[string]PeerReport)}

func (s *Store) Set(r PeerReport) {
	r.UpdatedAt = time.Now()
	s.mu.Lock()
	s.reports[r.PeerID] = r
	s.mu.Unlock()
}

func (s *Store) Delete(peerID string) {
	s.mu.Lock()
	delete(s.reports, peerID)
	s.mu.Unlock()
}

func (s *Store) Aggregate() Aggregate {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var agg Aggregate
	var rttSum, lossSum float64
	var rttN int

	for _, r := range s.reports {
		switch r.Quality {
		case "good":
			agg.PeersGood++
		case "medium":
			agg.PeersMedium++
		case "poor":
			agg.PeersPoor++
		}
		if r.RoundTripTimeMs >= 0 {
			rttSum += r.RoundTripTimeMs
			rttN++
		}
		lossSum += r.PacketLossPercent
		if r.CandidateType == "relay" {
			agg.RelayCount++
		}
	}

	n := len(s.reports)
	if n > 0 {
		agg.AvgLossPct = lossSum / float64(n)
	}
	if rttN > 0 {
		agg.AvgRttMs = rttSum / float64(rttN)
	}

	return agg
}
