package metrics

import (
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
)

type Snapshot struct {
	ActivePeers      float64 `json:"active_peers"`
	ActiveRooms      float64 `json:"active_rooms"`
	JoinsTotal       float64 `json:"joins_total"`
	IceRequestsTotal float64 `json:"ice_requests_total"`
	IceErrorsTotal   float64 `json:"ice_errors_total"`
	SignalsOffer     float64 `json:"signals_offer"`
	SignalsAnswer    float64 `json:"signals_answer"`
	SignalsCandidate float64 `json:"signals_candidate"`
}

func Gather() Snapshot {
	mfs, _ := prometheus.DefaultGatherer.Gather()
	raw := make(map[string]float64)
	for _, mf := range mfs {
		name := mf.GetName()
		if !strings.HasPrefix(name, "vartalaap_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			key := name
			for _, lp := range m.GetLabel() {
				key += ":" + lp.GetValue()
			}
			switch mf.GetType() {
			case dto.MetricType_GAUGE:
				raw[key] = m.GetGauge().GetValue()
			case dto.MetricType_COUNTER:
				raw[key] = m.GetCounter().GetValue()
			}
		}
	}
	return Snapshot{
		ActivePeers:      raw["vartalaap_active_peers"],
		ActiveRooms:      raw["vartalaap_active_rooms"],
		JoinsTotal:       raw["vartalaap_joins_total"],
		IceRequestsTotal: raw["vartalaap_ice_requests_total"],
		IceErrorsTotal:   raw["vartalaap_ice_errors_total"],
		SignalsOffer:     raw["vartalaap_signals_total:offer"],
		SignalsAnswer:    raw["vartalaap_signals_total:answer"],
		SignalsCandidate: raw["vartalaap_signals_total:candidate"],
	}
}
