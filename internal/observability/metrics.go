package observability

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Registry             *prometheus.Registry
	Sessions             prometheus.Gauge
	ConnectedSessions    prometheus.Gauge
	Registrations        *prometheus.GaugeVec
	Calls                *prometheus.GaugeVec
	CallsEnded           *prometheus.CounterVec
	CallSetupLatency     *prometheus.HistogramVec
	Commands             *prometheus.CounterVec
	Capabilities         *prometheus.CounterVec
	PeerConnections      prometheus.Gauge
	ICERoutes            *prometheus.GaugeVec
	TURNAllocations      prometheus.Gauge
	TURNQuotaUse         prometheus.Gauge
	TURNDenials          *prometheus.CounterVec
	TURNBytes            *prometheus.CounterVec
	WSSConnections       prometheus.Gauge
	WSSWriterQueueDepth  prometheus.Gauge
	WSSDisconnects       *prometheus.CounterVec
	LeaseEvents          *prometheus.CounterVec
	RTPPackets           *prometheus.CounterVec
	RTPReportedLoss      *prometheus.CounterVec
	RTPJitter            *prometheus.HistogramVec
	RTPBridgeErrors      *prometheus.CounterVec
	RTPSourceEvents      *prometheus.CounterVec
	OutboxDepth          prometheus.Gauge
	OutboxCapacityRatio  prometheus.Gauge
	SQLiteDatabaseBytes  prometheus.Gauge
	SQLiteWALBytes       prometheus.Gauge
	SQLiteCheckpointBusy prometheus.Gauge
	SQLiteFailures       *prometheus.CounterVec
	rtpBrowserToPBX      prometheus.Counter
	rtpPBXToBrowser      prometheus.Counter
	rtpUnknownDirection  prometheus.Counter
	turnMu               sync.Mutex
	lastTURN             TURNStats
}

type CallGauge struct {
	State     string
	Direction string
	Count     int
}

type TURNStats struct {
	Allocations           int
	AllocationLimit       int
	AuthenticationDenials uint64
	QuotaDenials          uint64
	BandwidthDenials      uint64
	PeerToClientBytes     uint64
	ClientToPeerBytes     uint64
}

type StorageStats struct {
	PendingEvents           int64
	MaxSessionPendingEvents int64
	DatabaseBytes           int64
	WALBytes                int64
	GlobalEventLimit        int
	PerSessionEventLimit    int
	MaxBytes                int64
}

func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	metrics := &Metrics{
		Registry: registry,
		Sessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "sessions",
			Help: "Current authenticated gateway sessions, including reconnect grace.",
		}),
		ConnectedSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "connected_sessions",
			Help: "Current gateway sessions with an attached WSS client.",
		}),
		Registrations: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "registrations",
			Help: "Current SIP registrations by bounded state.",
		}, []string{"state"}),
		Calls: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "calls",
			Help: "Current calls by bounded state and direction.",
		}, []string{"state", "direction"}),
		CallsEnded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "calls_ended_total",
			Help: "Terminal calls by bounded direction and normalized outcome.",
		}, []string{"direction", "outcome"}),
		CallSetupLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "call_setup_seconds",
			Help:    "Time from call creation to connected state.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 20, 30},
		}, []string{"direction"}),
		Commands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "commands_total",
			Help: "Gateway commands by bounded type and outcome.",
		}, []string{"type", "outcome"}),
		Capabilities: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "capability_verifications_total",
			Help: "Locally verified encrypted capabilities by bounded kind and outcome.",
		}, []string{"kind", "outcome"}),
		PeerConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "peer_connections",
			Help: "Current WebRTC PeerConnections.",
		}),
		ICERoutes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "ice_routes",
			Help: "Current WebRTC PeerConnections by bounded selected ICE route type.",
		}, []string{"route"}),
		TURNAllocations: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "turn_allocations",
			Help: "Current embedded TURN allocations.",
		}),
		TURNQuotaUse: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "turn_allocation_quota_ratio",
			Help: "Fraction of the embedded TURN allocation quota currently in use.",
		}),
		TURNDenials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "turn_denials_total",
			Help: "Embedded TURN denials by bounded reason.",
		}, []string{"reason"}),
		TURNBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "turn_relay_bytes_total",
			Help: "Bytes forwarded through embedded TURN relay sockets by direction.",
		}, []string{"direction"}),
		WSSConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "wss_connections",
			Help: "Current authenticated WSS connections.",
		}),
		WSSWriterQueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "wss_writer_queue_depth",
			Help: "Current queued WSS messages across connected clients.",
		}),
		WSSDisconnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "wss_disconnects_total",
			Help: "WSS disconnects by bounded reason.",
		}, []string{"reason"}),
		LeaseEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "lease_events_total",
			Help: "Gateway session lease events by bounded outcome.",
		}, []string{"outcome"}),
		RTPPackets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "rtp_packets_total",
			Help: "RTP packets successfully bridged by direction.",
		}, []string{"direction"}),
		RTPReportedLoss: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "rtp_reported_loss_total",
			Help: "Positive cumulative packet-loss deltas reported through RTCP.",
		}, []string{"direction"}),
		RTPJitter: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "rtp_jitter_seconds",
			Help:    "Interarrival jitter reported through RTCP for 8 kHz audio.",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5},
		}, []string{"direction"}),
		RTPBridgeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "rtp_bridge_errors_total",
			Help: "RTP or RTCP bridge errors by bounded direction.",
		}, []string{"direction"}),
		RTPSourceEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "rtp_source_events_total",
			Help: "PBX RTP or RTCP source validation and controlled port-learning events.",
		}, []string{"protocol", "outcome"}),
		OutboxDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "outbox_pending_events",
			Help: "Current durable unacknowledged event count.",
		}),
		OutboxCapacityRatio: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "outbox_capacity_ratio",
			Help: "Highest durable outbox pressure ratio across global events, per-session events, and SQLite bytes.",
		}),
		SQLiteDatabaseBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "sqlite_database_bytes",
			Help: "Current SQLite database and shared-memory bytes.",
		}),
		SQLiteWALBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "sqlite_wal_bytes",
			Help: "Current SQLite WAL bytes.",
		}),
		SQLiteCheckpointBusy: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "sqlite_checkpoint_busy",
			Help: "Whether the last passive SQLite checkpoint reported busy readers.",
		}),
		SQLiteFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "debt_flow", Subsystem: "sip_gateway", Name: "sqlite_failures_total",
			Help: "SQLite maintenance or statistics failures by bounded operation.",
		}, []string{"operation"}),
	}
	registry.MustRegister(
		metrics.Sessions, metrics.ConnectedSessions, metrics.Registrations, metrics.Calls,
		metrics.CallsEnded, metrics.CallSetupLatency, metrics.Commands, metrics.Capabilities,
		metrics.PeerConnections, metrics.ICERoutes, metrics.TURNAllocations, metrics.TURNQuotaUse,
		metrics.TURNDenials, metrics.TURNBytes, metrics.WSSConnections, metrics.WSSWriterQueueDepth,
		metrics.WSSDisconnects, metrics.LeaseEvents, metrics.RTPPackets, metrics.RTPReportedLoss,
		metrics.RTPJitter, metrics.RTPBridgeErrors, metrics.RTPSourceEvents,
		metrics.OutboxDepth, metrics.OutboxCapacityRatio,
		metrics.SQLiteDatabaseBytes, metrics.SQLiteWALBytes, metrics.SQLiteCheckpointBusy,
		metrics.SQLiteFailures,
	)
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	metrics.rtpBrowserToPBX = metrics.RTPPackets.WithLabelValues("browser_to_pbx")
	metrics.rtpPBXToBrowser = metrics.RTPPackets.WithLabelValues("pbx_to_browser")
	metrics.rtpUnknownDirection = metrics.RTPPackets.WithLabelValues("unknown")
	return metrics
}

func (m *Metrics) ObserveCommand(commandType, outcome string) {
	m.Commands.WithLabelValues(commandType, outcome).Inc()
}

func (m *Metrics) ObserveCapability(kind, outcome string) {
	m.Capabilities.WithLabelValues(kind, outcome).Inc()
}

func (m *Metrics) ObserveCallConnected(direction string, latency time.Duration) {
	m.CallSetupLatency.WithLabelValues(direction).Observe(latency.Seconds())
}

func (m *Metrics) ObserveCallEnded(direction, outcome string) {
	m.CallsEnded.WithLabelValues(direction, outcome).Inc()
}

func (m *Metrics) ObserveRTPPacket(direction string) {
	switch direction {
	case "browser_to_pbx":
		m.rtpBrowserToPBX.Inc()
	case "pbx_to_browser":
		m.rtpPBXToBrowser.Inc()
	default:
		m.rtpUnknownDirection.Inc()
	}
}

func (m *Metrics) ObserveRTPQuality(direction string, lost, jitter uint32) {
	if lost > 0 {
		m.RTPReportedLoss.WithLabelValues(direction).Add(float64(lost))
	}
	m.RTPJitter.WithLabelValues(direction).Observe(float64(jitter) / 8000)
}

func (m *Metrics) ObserveRTPBridgeError(direction string) {
	m.RTPBridgeErrors.WithLabelValues(direction).Inc()
}

func (m *Metrics) ObserveRTPSource(protocol, outcome string) {
	m.RTPSourceEvents.WithLabelValues(protocol, outcome).Inc()
}

func (m *Metrics) ObserveLease(outcome string) {
	m.LeaseEvents.WithLabelValues(outcome).Inc()
}

func (m *Metrics) ObserveWSSDisconnect(reason string) {
	m.WSSDisconnects.WithLabelValues(reason).Inc()
}

func (m *Metrics) SetRuntime(authenticated, connected int, registrations map[string]int, calls []CallGauge) {
	m.Sessions.Set(float64(authenticated))
	m.ConnectedSessions.Set(float64(connected))
	m.Registrations.Reset()
	for state, count := range registrations {
		m.Registrations.WithLabelValues(state).Set(float64(count))
	}
	m.Calls.Reset()
	for _, value := range calls {
		m.Calls.WithLabelValues(value.State, value.Direction).Set(float64(value.Count))
	}
}

func (m *Metrics) SetInfrastructure(peerConnections, wssConnections, writerQueue int) {
	m.PeerConnections.Set(float64(peerConnections))
	m.WSSConnections.Set(float64(wssConnections))
	m.WSSWriterQueueDepth.Set(float64(writerQueue))
}

func (m *Metrics) SetICERoutes(routes map[string]int) {
	m.ICERoutes.Reset()
	for route, count := range routes {
		m.ICERoutes.WithLabelValues(route).Set(float64(count))
	}
}

func (m *Metrics) SetTURN(value TURNStats) {
	m.TURNAllocations.Set(float64(value.Allocations))
	quotaRatio := 0.0
	if value.AllocationLimit > 0 {
		quotaRatio = float64(value.Allocations) / float64(value.AllocationLimit)
	}
	m.TURNQuotaUse.Set(quotaRatio)
	m.turnMu.Lock()
	m.TURNDenials.WithLabelValues("authentication").Add(
		delta(value.AuthenticationDenials, m.lastTURN.AuthenticationDenials))
	m.TURNDenials.WithLabelValues("allocation_quota").Add(
		delta(value.QuotaDenials, m.lastTURN.QuotaDenials))
	m.TURNDenials.WithLabelValues("bandwidth_quota").Add(
		delta(value.BandwidthDenials, m.lastTURN.BandwidthDenials))
	m.TURNBytes.WithLabelValues("peer_to_client").Add(
		delta(value.PeerToClientBytes, m.lastTURN.PeerToClientBytes))
	m.TURNBytes.WithLabelValues("client_to_peer").Add(
		delta(value.ClientToPeerBytes, m.lastTURN.ClientToPeerBytes))
	m.lastTURN = value
	m.turnMu.Unlock()
}

func (m *Metrics) SetStorage(value StorageStats) {
	m.OutboxDepth.Set(float64(value.PendingEvents))
	m.OutboxCapacityRatio.Set(maxRatio(
		ratio(value.PendingEvents, int64(value.GlobalEventLimit)),
		ratio(value.MaxSessionPendingEvents, int64(value.PerSessionEventLimit)),
		ratio(value.DatabaseBytes+value.WALBytes, value.MaxBytes),
	))
	m.SQLiteDatabaseBytes.Set(float64(value.DatabaseBytes))
	m.SQLiteWALBytes.Set(float64(value.WALBytes))
}

func ratio(value, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	return float64(value) / float64(limit)
}

func maxRatio(values ...float64) float64 {
	maximum := 0.0
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func (m *Metrics) SetCheckpointBusy(busy int) {
	m.SQLiteCheckpointBusy.Set(float64(busy))
}

func (m *Metrics) ObserveSQLiteFailure(operation string) {
	m.SQLiteFailures.WithLabelValues(operation).Inc()
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

func delta(current, previous uint64) float64 {
	if current >= previous {
		return float64(current - previous)
	}
	return float64(current)
}
