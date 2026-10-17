package observability

import (
	"testing"
	"time"
)

func TestMetricsExposeRuntimeAndInfrastructureState(t *testing.T) {
	metrics := NewMetrics()
	metrics.SetRuntime(3, 2, map[string]int{"registered": 2, "idle": 1}, []CallGauge{{
		State: "connected", Direction: "outbound", Count: 1,
	}})
	metrics.SetInfrastructure(2, 2, 4)
	metrics.SetICERoutes(map[string]int{"direct": 1, "relay": 1})
	metrics.SetTURN(TURNStats{
		Allocations: 1, AllocationLimit: 4, AuthenticationDenials: 2,
		QuotaDenials: 1, BandwidthDenials: 3, PeerToClientBytes: 1024,
		ClientToPeerBytes: 2048,
	})
	metrics.ObserveCallConnected("outbound", 750*time.Millisecond)
	metrics.ObserveCallEnded("outbound", "remote_hangup")
	metrics.ObserveRTPPacket("browser_to_pbx")
	metrics.ObserveRTPQuality("browser_to_pbx", 2, 80)
	metrics.ObserveRTPBridgeError("pbx_to_browser")
	metrics.ObserveLease("refreshed")
	metrics.ObserveWSSDisconnect("client_closed")
	metrics.SetStorage(StorageStats{
		PendingEvents: 7, MaxSessionPendingEvents: 6,
		DatabaseBytes: 1024, WALBytes: 256,
		GlobalEventLimit: 10, PerSessionEventLimit: 10, MaxBytes: 10_000,
	})
	metrics.SetCheckpointBusy(1)
	metrics.ObserveSQLiteFailure("maintenance")

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]float64)
	for _, family := range families {
		if len(family.Metric) == 1 && family.Metric[0].Gauge != nil {
			values[family.GetName()] = family.Metric[0].Gauge.GetValue()
		}
	}
	for name, expected := range map[string]float64{
		"debt_flow_sip_gateway_sessions":                    3,
		"debt_flow_sip_gateway_connected_sessions":          2,
		"debt_flow_sip_gateway_peer_connections":            2,
		"debt_flow_sip_gateway_turn_allocations":            1,
		"debt_flow_sip_gateway_turn_allocation_quota_ratio": 0.25,
		"debt_flow_sip_gateway_wss_connections":             2,
		"debt_flow_sip_gateway_wss_writer_queue_depth":      4,
		"debt_flow_sip_gateway_outbox_pending_events":       7,
		"debt_flow_sip_gateway_outbox_capacity_ratio":       0.7,
		"debt_flow_sip_gateway_sqlite_database_bytes":       1024,
		"debt_flow_sip_gateway_sqlite_wal_bytes":            256,
		"debt_flow_sip_gateway_sqlite_checkpoint_busy":      1,
	} {
		if values[name] != expected {
			t.Errorf("%s=%v, want %v", name, values[name], expected)
		}
	}
	names := make(map[string]bool)
	for _, family := range families {
		names[family.GetName()] = true
	}
	for _, name := range []string{
		"debt_flow_sip_gateway_call_setup_seconds",
		"debt_flow_sip_gateway_calls_ended_total",
		"debt_flow_sip_gateway_ice_routes",
		"debt_flow_sip_gateway_turn_denials_total",
		"debt_flow_sip_gateway_turn_relay_bytes_total",
		"debt_flow_sip_gateway_wss_disconnects_total",
		"debt_flow_sip_gateway_lease_events_total",
		"debt_flow_sip_gateway_rtp_packets_total",
		"debt_flow_sip_gateway_rtp_reported_loss_total",
		"debt_flow_sip_gateway_rtp_jitter_seconds",
		"debt_flow_sip_gateway_rtp_bridge_errors_total",
		"debt_flow_sip_gateway_sqlite_failures_total",
	} {
		if !names[name] {
			t.Errorf("missing metric family %s", name)
		}
	}
}

func TestStorageCapacityRatioUsesHighestConfiguredBound(t *testing.T) {
	metrics := NewMetrics()
	metrics.SetStorage(StorageStats{
		PendingEvents: 20, MaxSessionPendingEvents: 9,
		DatabaseBytes: 600, WALBytes: 350,
		GlobalEventLimit: 100, PerSessionEventLimit: 20, MaxBytes: 1000,
	})
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "debt_flow_sip_gateway_outbox_capacity_ratio" {
			if got := family.Metric[0].Gauge.GetValue(); got != 0.95 {
				t.Fatalf("capacity ratio=%v, want 0.95 from SQLite byte pressure", got)
			}
			return
		}
	}
	t.Fatal("missing outbox capacity ratio metric")
}
