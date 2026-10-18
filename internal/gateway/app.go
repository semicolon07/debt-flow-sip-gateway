package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/debt-flow/sip-gateway/internal/capability"
	"github.com/debt-flow/sip-gateway/internal/config"
	"github.com/debt-flow/sip-gateway/internal/diagnostics"
	"github.com/debt-flow/sip-gateway/internal/media"
	"github.com/debt-flow/sip-gateway/internal/observability"
	"github.com/debt-flow/sip-gateway/internal/security"
	"github.com/debt-flow/sip-gateway/internal/sip"
	"github.com/debt-flow/sip-gateway/internal/store"
	gatewayturn "github.com/debt-flow/sip-gateway/internal/turn"
	gatewaywebrtc "github.com/debt-flow/sip-gateway/internal/webrtc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

func Run(ctx context.Context, cfg config.Config) error {
	return RunWithVersion(ctx, cfg, "dev")
}

func RunWithVersion(ctx context.Context, cfg config.Config, version string) error {
	logger, logCloser, err := observability.NewLogger(
		cfg.Logging.Path, cfg.Logging.MaxBytes, cfg.Logging.MaxBackups, cfg.Logging.Level,
	)
	if err != nil {
		return fmt.Errorf("open operational log: %w", err)
	}
	defer logCloser.Close()
	slog.SetDefault(logger)
	if cfg.NodeID == "" {
		return errors.New("nodeId is required to run the gateway")
	}
	keys, err := capability.LoadKeys(cfg.Capability, security.NewMachineProtector())
	if err != nil {
		return fmt.Errorf("load capability keys: %w", err)
	}
	capabilityCodec, err := capability.New(cfg.NodeID, cfg.Capability.Issuer, cfg.Capability.Audience, keys)
	for _, key := range keys {
		security.Zero(key)
	}
	if err != nil {
		return fmt.Errorf("create capability verifier: %w", err)
	}
	defer capabilityCodec.Close()

	database, err := store.Open(ctx, cfg.Storage.Path, store.Limits{
		PerSession: cfg.Storage.MaxPendingSession,
		Global:     cfg.Storage.MaxPendingGlobal,
		MaxBytes:   cfg.Storage.MaxBytes,
	})
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.CheckIntegrity(ctx); err != nil {
		return fmt.Errorf("verify gateway database: %w", err)
	}
	metrics := observability.NewMetrics()
	recoveredCommands, err := database.RecoverInterruptedCommands(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("recover interrupted commands: %w", err)
	}
	if recoveredCommands > 0 {
		logger.Warn("terminalized commands interrupted by previous gateway process", "count", recoveredCommands)
	}
	recovered, err := database.RecoverRestartedCalls(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("recover active calls: %w", err)
	}
	if recovered > 0 {
		logger.Warn("terminalized calls interrupted by previous gateway process", "count", recovered)
	}
	maintainDatabase := func(maintenanceCtx context.Context) {
		now := time.Now().UTC()
		result, maintenanceErr := database.Maintain(
			maintenanceCtx, now.AddDate(0, 0, -cfg.Storage.RetentionDays), now,
		)
		if maintenanceErr != nil {
			metrics.ObserveSQLiteFailure("maintenance")
			logger.Error("gateway database maintenance failed", "error", maintenanceErr)
			return
		}
		metrics.SetCheckpointBusy(result.CheckpointBusy)
		if stats, statsErr := database.Stats(maintenanceCtx); statsErr == nil {
			setStorageMetrics(metrics, cfg, stats)
		} else {
			metrics.ObserveSQLiteFailure("stats")
			logger.Warn("read gateway database metrics failed", "error", statsErr)
		}
		if result.PurgedRows > 0 || result.CheckpointBusy != 0 {
			logger.Info("gateway database maintenance completed",
				"purgedRows", result.PurgedRows, "checkpointBusy", result.CheckpointBusy,
				"walFrames", result.WALFrames, "checkpointedFrames", result.CheckpointedFrames,
			)
		}
	}
	maintainDatabase(ctx)
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				maintainDatabase(ctx)
			}
		}
	}()

	turnCredentials, err := gatewayturn.NewCredentials(cfg.TURN.CredentialTTL)
	if err != nil {
		return err
	}
	defer turnCredentials.Close()
	turnServer, err := gatewayturn.Start(gatewayturn.Config{
		ListenUDP: cfg.TURN.ListenUDP, ListenTCP: cfg.TURN.ListenTCP,
		AdvertisedIP: net.ParseIP(cfg.TURN.AdvertisedIP), Realm: cfg.TURN.Realm,
		RelayMinPort: cfg.TURN.RelayMinPort, RelayMaxPort: cfg.TURN.RelayMaxPort,
		MaxAllocations: cfg.TURN.MaxAllocations, PerUserLimit: 1,
		BytesPerSecond: cfg.TURN.BytesPerSecond,
	}, turnCredentials)
	if err != nil {
		return fmt.Errorf("start embedded TURN: %w", err)
	}
	defer turnServer.Close()

	turnPort, err := portOf(cfg.TURN.ListenUDP)
	if err != nil {
		return err
	}
	turnURLs := []string{
		fmt.Sprintf("turn:%s:%s?transport=udp", cfg.TURN.AdvertisedIP, turnPort),
		fmt.Sprintf("turn:%s:%s?transport=tcp", cfg.TURN.AdvertisedIP, turnPort),
	}
	var coordinator *Coordinator
	rtcEngine, err := gatewaywebrtc.New(gatewaywebrtc.Config{
		UDPMuxListen: cfg.ICE.UDPMuxListen, AdvertisedIP: cfg.ICE.AdvertisedIP,
		TURNURLs: turnURLs, MaxSessions: cfg.Limits.MaxSessions,
	}, func(sessionID string, candidate pion.ICECandidateInit) {
		if coordinator != nil {
			coordinator.OnICECandidate(sessionID, candidate)
		}
	}, func(sessionID string, packet *rtp.Packet) {
		if coordinator != nil {
			coordinator.OnBrowserRTP(sessionID, packet)
		}
	}, func(sessionID string, packets []rtcp.Packet) {
		if coordinator != nil {
			coordinator.OnBrowserRTCP(sessionID, packets)
		}
	}, func(sessionID string, state pion.PeerConnectionState) {
		if coordinator != nil {
			coordinator.OnWebRTCState(sessionID, state)
		}
	})
	if err != nil {
		return err
	}
	defer rtcEngine.Close()

	rtpListenHost, _, err := net.SplitHostPort(cfg.SIP.ListenUDP)
	if err != nil {
		return err
	}
	rtpManager, err := media.NewRTPManager(media.RTPConfig{
		ListenIP: net.ParseIP(rtpListenHost), AdvertisedIP: net.ParseIP(cfg.SIP.RTPAdvertisedIP),
		MinPort: cfg.SIP.RTPMinPort, MaxPort: cfg.SIP.RTPMaxPort,
	}, func(callID string, packet *rtp.Packet) {
		if coordinator != nil {
			coordinator.OnPBXRTP(callID, packet)
		}
	}, func(callID string, packets []rtcp.Packet) {
		if coordinator != nil {
			coordinator.OnPBXRTCP(callID, packets)
		}
	}, func(callID, protocol, outcome string, expected, actual *net.UDPAddr) {
		metrics.ObserveRTPSource(protocol, outcome)
		if outcome == "migrated" {
			logger.Info("PBX media source port migrated",
				"callId", callID, "protocol", protocol,
				"previous", expected.String(), "actual", actual.String(),
			)
		}
	})
	if err != nil {
		return fmt.Errorf("create RTP manager: %w", err)
	}
	defer rtpManager.Close()

	var sipRuntime sip.Runtime = sip.NewGatedRuntime()
	if cfg.SIP.SpikeApproved {
		sipRuntime, err = sip.NewSIPGoRuntime(sip.SIPGoConfig{
			ListenUDP: cfg.SIP.ListenUDP, ListenTCP: cfg.SIP.ListenTCP,
			AdvertisedIP: cfg.SIP.RTPAdvertisedIP, UserAgent: "DebtFlowSipGateway/1",
			Logger: logger, Debug: cfg.Logging.Level == "debug",
			CanAcceptIncoming: func(sessionID string) bool {
				return coordinator != nil && coordinator.CanAcceptIncoming(sessionID)
			},
		})
		if err != nil {
			return fmt.Errorf("start SIP runtime: %w", err)
		}
	}
	coordinator, err = NewCoordinator(CoordinatorConfig{
		NodeID: cfg.NodeID, MaxSessions: cfg.Limits.MaxSessions, MaxCalls: cfg.Limits.MaxCalls,
		TURNURLs: turnURLs, LeaseRenewEvery: cfg.Lease.RenewEvery,
		LeaseExpiresAfter: cfg.Lease.ExpiresAfter, ReconnectGrace: cfg.Lease.ReconnectGrace,
	}, capabilityCodec, database, turnCredentials, rtcEngine, rtpManager, sipRuntime, logger)
	if err != nil {
		return err
	}
	coordinator.SetObserver(metrics)
	defer coordinator.Close()
	wss, err := NewServer(ServerConfig{
		AllowedOrigins:              cfg.HTTP.EffectiveAllowedOrigins(),
		AllowInsecureLoopbackOrigin: cfg.HTTP.AllowInsecureLoopbackOrigin,
		AuthTimeout:                 cfg.HTTP.AuthTimeout,
		Heartbeat:                   cfg.HTTP.Heartbeat, IdleTimeout: cfg.HTTP.IdleTimeout,
		ReconnectGrace: cfg.Lease.ReconnectGrace, MaxMessageBytes: cfg.Limits.MaxMessageBytes,
		MessagesPerWindow: cfg.Limits.MessagesPerWindow, MessageWindow: cfg.Limits.MessageWindow,
		WriterQueue: cfg.Limits.WriterQueue,
	}, coordinator, coordinator, logger)
	if err != nil {
		return err
	}
	wss.SetObserver(metrics)
	var live atomic.Bool
	live.Store(true)
	sipReady := func() bool {
		value, ok := sipRuntime.(interface{ Ready() bool })
		return cfg.SIP.SpikeApproved && ok && value.Ready()
	}
	var diagnosticsSnapshot atomic.Value

	updateMetrics := func() {
		runtimeStats := coordinator.Stats()
		callGauges := make([]observability.CallGauge, 0, len(runtimeStats.Calls))
		diagnosticCalls := make([]diagnostics.Call, 0, len(runtimeStats.Calls))
		activeCalls := 0
		for _, value := range runtimeStats.Calls {
			activeCalls += value.Count
			callGauges = append(callGauges, observability.CallGauge{
				State: value.State, Direction: value.Direction, Count: value.Count,
			})
			diagnosticCalls = append(diagnosticCalls, diagnostics.Call{
				State: value.State, Direction: value.Direction, Count: value.Count,
			})
		}
		metrics.SetRuntime(
			runtimeStats.AuthenticatedSessions, runtimeStats.ConnectedSessions,
			runtimeStats.Registrations, callGauges,
		)
		metrics.SetCapacity(
			sipReady(), runtimeStats.AuthenticatedSessions, cfg.Limits.MaxSessions,
			activeCalls, cfg.Limits.MaxCalls,
		)
		serverStats := wss.Stats()
		webrtcStats := rtcEngine.Stats()
		metrics.SetInfrastructure(
			webrtcStats.Sessions, serverStats.Connections, serverStats.WriterQueueDepth,
		)
		metrics.SetICERoutes(webrtcStats.ICERoutes)
		turnStats := turnServer.Stats()
		metrics.SetTURN(observability.TURNStats{
			Allocations: turnStats.Allocations, AllocationLimit: turnStats.AllocationLimit,
			AuthenticationDenials: turnStats.AuthenticationDenials,
			QuotaDenials:          turnStats.QuotaDenials, BandwidthDenials: turnStats.BandwidthDenials,
			PeerToClientBytes: turnStats.PeerToClientBytes,
			ClientToPeerBytes: turnStats.ClientToPeerBytes,
		})
		storageStats, statsErr := database.Stats(ctx)
		storageHealthy := statsErr == nil
		if storageHealthy {
			setStorageMetrics(metrics, cfg, storageStats)
		} else {
			metrics.ObserveSQLiteFailure("stats")
			logger.Warn("read gateway database metrics failed", "error", statsErr)
		}
		turnQuotaRatio := 0.0
		if turnStats.AllocationLimit > 0 {
			turnQuotaRatio = float64(turnStats.Allocations) / float64(turnStats.AllocationLimit)
		}
		processStats, processStatsErr := metrics.ProcessStats()
		if processStatsErr != nil {
			logger.Warn("read process metrics failed", "error", processStatsErr)
		}
		diagnosticsSnapshot.Store(diagnostics.Snapshot{
			GeneratedAtUTC: time.Now().UTC(), NodeID: cfg.NodeID, Version: version, Ready: sipReady(),
			Limits: diagnostics.WebLimits{Sessions: cfg.Limits.MaxSessions, Calls: cfg.Limits.MaxCalls},
			Runtime: diagnostics.WebRuntime{
				AuthenticatedSessions: runtimeStats.AuthenticatedSessions,
				ConnectedSessions:     runtimeStats.ConnectedSessions,
				Registrations:         runtimeStats.Registrations, Calls: diagnosticCalls,
			},
			Infrastructure: diagnostics.Infrastructure{
				PeerConnections: webrtcStats.Sessions, WSSConnections: serverStats.Connections,
				WSSWriterQueue: serverStats.WriterQueueDepth, ICERoutes: webrtcStats.ICERoutes,
			},
			TURN: diagnostics.WebTURN{
				Allocations: turnStats.Allocations, AllocationLimit: turnStats.AllocationLimit,
				QuotaRatio: turnQuotaRatio,
			},
			Storage: diagnostics.WebStorage{
				Healthy: storageHealthy, PendingEvents: storageStats.PendingEvents,
				MaxSessionPendingEvents: storageStats.MaxSessionPendingEvents,
				DatabaseBytes:           storageStats.DatabaseBytes, WALBytes: storageStats.WALBytes,
			},
			Process: diagnostics.Process{
				CPUSeconds: processStats.CPUSeconds, CPUCores: processStats.CPUCores,
				ResidentMemoryBytes: processStats.ResidentMemoryBytes,
				VirtualMemoryBytes:  processStats.VirtualMemoryBytes,
			},
		})
	}
	updateMetrics()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				updateMetrics()
			}
		}
	}()
	mainMux := http.NewServeMux()
	mainMux.Handle("/gateway/v1", wss.Handler())
	mainMux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		if !live.Load() {
			http.Error(writer, "stopping", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"live"}`))
	})
	mainMux.HandleFunc("/readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if !sipReady() {
			http.Error(writer, "SIP runtime is not ready", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"ready"}`))
	})
	mainServer := &http.Server{
		Addr: cfg.HTTP.Listen, Handler: mainMux,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metrics.Handler())
	diagnosticsHandler := diagnostics.NewWebHandler(func() diagnostics.Snapshot {
		value, ok := diagnosticsSnapshot.Load().(diagnostics.Snapshot)
		if !ok {
			return diagnostics.Snapshot{NodeID: cfg.NodeID, Version: version, Ready: sipReady()}
		}
		value.Ready = sipReady()
		return value
	})
	metricsMux.Handle("/diagnostics", diagnosticsHandler)
	metricsMux.Handle("/diagnostics/", diagnosticsHandler)
	metricsServer := &http.Server{
		Addr: cfg.HTTP.MetricsListen, Handler: metricsMux,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
	}

	errorsChannel := make(chan error, 2)
	go serveHTTP(mainServer, errorsChannel)
	go serveHTTP(metricsServer, errorsChannel)
	logger.Info("SIP gateway service started",
		"nodeId", cfg.NodeID, "httpListen", cfg.HTTP.Listen,
		"metricsListen", cfg.HTTP.MetricsListen, "ready", sipReady(),
	)
	select {
	case <-ctx.Done():
	case err := <-errorsChannel:
		if err != nil {
			return err
		}
	}
	live.Store(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	return errors.Join(mainServer.Shutdown(shutdownCtx), metricsServer.Shutdown(shutdownCtx))
}

func setStorageMetrics(metrics *observability.Metrics, cfg config.Config, stats store.Stats) {
	metrics.SetStorage(observability.StorageStats{
		PendingEvents: stats.PendingEvents, MaxSessionPendingEvents: stats.MaxSessionPendingEvents,
		DatabaseBytes: stats.DatabaseBytes, WALBytes: stats.WALBytes,
		GlobalEventLimit:     cfg.Storage.MaxPendingGlobal,
		PerSessionEventLimit: cfg.Storage.MaxPendingSession,
		MaxBytes:             cfg.Storage.MaxBytes,
	})
}

func serveHTTP(server *http.Server, result chan<- error) {
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	result <- err
}

func portOf(address string) (string, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(port), nil
}
