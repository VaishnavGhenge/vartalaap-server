package signaling

import (
	"encoding/json"
	"testing"
)

func TestStatsReportAcceptsBrowserFractionalBitrates(t *testing.T) {
	var report StatsReportData
	err := json.Unmarshal([]byte(`{"peers":[{"peerId":"remote","roundTripTimeMs":23.5,"outboundBitrateKbps":512.42,"inboundBitrateKbps":498.13,"packetLossPercent":0.1,"jitterMs":2.5}]}`), &report)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Peers) != 1 || report.Peers[0].OutboundBitrateKbps != 512.42 || report.Peers[0].InboundBitrateKbps != 498.13 {
		t.Fatalf("browser bitrate precision was lost: %+v", report)
	}
}
