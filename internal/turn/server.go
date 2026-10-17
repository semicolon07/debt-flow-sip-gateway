package turn

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	pionturn "github.com/pion/turn/v5"
	"golang.org/x/time/rate"
)

type Config struct {
	ListenUDP      string
	ListenTCP      string
	AdvertisedIP   net.IP
	Realm          string
	RelayMinPort   uint16
	RelayMaxPort   uint16
	MaxAllocations int
	PerUserLimit   int
	BytesPerSecond int
}

type Server struct {
	server          *pionturn.Server
	credentials     *Credentials
	quota           *quota
	limiters        *userRateLimiters
	authDenied      atomic.Uint64
	bytesRead       atomic.Uint64
	bytesWritten    atomic.Uint64
	bandwidthDenied atomic.Uint64
}

type quota struct {
	mu           sync.Mutex
	total        int
	byUser       map[string]int
	reservations map[string]quotaReservation
	maxTotal     int
	maxByUser    int
	denied       atomic.Uint64
}

type quotaReservation struct {
	userID    string
	expiresAt time.Time
}

type Stats struct {
	Allocations           int
	AllocationLimit       int
	AuthenticationDenials uint64
	QuotaDenials          uint64
	BandwidthDenials      uint64
	PeerToClientBytes     uint64
	ClientToPeerBytes     uint64
}

type userRateLimiters struct {
	values sync.Map
	limit  rate.Limit
	burst  int
}

type meteredPacketConn struct {
	net.PacketConn
	limiter         *rate.Limiter
	readBytes       *atomic.Uint64
	writtenBytes    *atomic.Uint64
	bandwidthDenied *atomic.Uint64
}

type relayGenerator struct {
	pionturn.RelayAddressGenerator
	limiters        *userRateLimiters
	readBytes       *atomic.Uint64
	writtenBytes    *atomic.Uint64
	bandwidthDenied *atomic.Uint64
}

func Start(cfg Config, credentials *Credentials) (*Server, error) {
	if credentials == nil {
		return nil, errors.New("TURN credentials are required")
	}
	if cfg.AdvertisedIP == nil || cfg.Realm == "" || cfg.RelayMinPort == 0 ||
		cfg.RelayMaxPort < cfg.RelayMinPort || cfg.BytesPerSecond < 1600 {
		return nil, errors.New("invalid TURN configuration")
	}
	udp, err := net.ListenPacket("udp4", cfg.ListenUDP)
	if err != nil {
		return nil, err
	}
	tcp, err := net.Listen("tcp4", cfg.ListenTCP)
	if err != nil {
		_ = udp.Close()
		return nil, err
	}
	q := &quota{
		byUser: make(map[string]int), reservations: make(map[string]quotaReservation),
		maxTotal: cfg.MaxAllocations, maxByUser: cfg.PerUserLimit,
	}
	limiters := &userRateLimiters{limit: rate.Limit(cfg.BytesPerSecond), burst: cfg.BytesPerSecond}
	result := &Server{credentials: credentials, quota: q, limiters: limiters}
	loggerFactory := logging.NewDefaultLoggerFactory()
	loggerFactory.DefaultLogLevel = logging.LogLevelDisabled
	loggerFactory.Writer = io.Discard
	newGenerator := func() pionturn.RelayAddressGenerator {
		base := &pionturn.RelayAddressGeneratorPortRange{
			RelayAddress: cfg.AdvertisedIP, Address: "0.0.0.0",
			MinPort: cfg.RelayMinPort, MaxPort: cfg.RelayMaxPort, MaxRetries: 32,
		}
		return &relayGenerator{
			RelayAddressGenerator: base, limiters: limiters,
			readBytes: &result.bytesRead, writtenBytes: &result.bytesWritten,
			bandwidthDenied: &result.bandwidthDenied,
		}
	}
	server, err := pionturn.NewServer(pionturn.ServerConfig{
		Realm: cfg.Realm,
		AuthHandler: func(attributes *pionturn.RequestAttributes) (string, []byte, bool) {
			userID, password, ok := credentials.Authenticate(attributes.Username, time.Now().UTC())
			if !ok {
				result.authDenied.Add(1)
				return "", nil, false
			}
			return userID, pionturn.GenerateAuthKey(attributes.Username, cfg.Realm, password), true
		},
		QuotaHandler: func(username, _ string, source net.Addr) bool {
			userID, _, ok := credentials.Authenticate(username, time.Now().UTC())
			return ok && q.allow(userID, source.String(), time.Now())
		},
		EventHandler: pionturn.EventHandler{
			OnAllocationCreated: func(source, _ net.Addr, _, userID, _ string, _ net.Addr, _ int) {
				q.created(userID, source.String())
			},
			OnAllocationDeleted: func(_, _ net.Addr, _, userID, realm string) {
				q.deleted(userID)
				limiters.remove(userID, realm)
			},
		},
		AllocationLifetime: 2 * time.Minute,
		PermissionTimeout:  2 * time.Minute,
		ChannelBindTimeout: 2 * time.Minute,
		LoggerFactory:      loggerFactory,
		PacketConnConfigs: []pionturn.PacketConnConfig{{
			PacketConn: udp, RelayAddressGenerator: newGenerator(),
		}},
		ListenerConfigs: []pionturn.ListenerConfig{{
			Listener: tcp, RelayAddressGenerator: newGenerator(),
		}},
	})
	if err != nil {
		_ = udp.Close()
		_ = tcp.Close()
		return nil, err
	}
	result.server = server
	return result, nil
}

func (s *Server) Close() error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Close()
}

func (s *Server) AllocationCount() int {
	if s == nil || s.quota == nil {
		return 0
	}
	s.quota.mu.Lock()
	defer s.quota.mu.Unlock()
	return s.quota.total
}

func (s *Server) Stats() Stats {
	if s == nil || s.quota == nil {
		return Stats{}
	}
	s.quota.mu.Lock()
	allocations := s.quota.total
	limit := s.quota.maxTotal
	s.quota.mu.Unlock()
	return Stats{
		Allocations: allocations, AllocationLimit: limit,
		AuthenticationDenials: s.authDenied.Load(), QuotaDenials: s.quota.denied.Load(),
		BandwidthDenials: s.bandwidthDenied.Load(), PeerToClientBytes: s.bytesRead.Load(),
		ClientToPeerBytes: s.bytesWritten.Load(),
	}
}

func (q *quota) allow(userID, source string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireReservations(now)
	if reservation, exists := q.reservations[source]; exists && reservation.userID == userID {
		return true
	}
	reservedByUser := 0
	for _, reservation := range q.reservations {
		if reservation.userID == userID {
			reservedByUser++
		}
	}
	if q.maxTotal > 0 && q.total+len(q.reservations) >= q.maxTotal {
		q.denied.Add(1)
		return false
	}
	allowed := q.maxByUser <= 0 || q.byUser[userID]+reservedByUser < q.maxByUser
	if !allowed {
		q.denied.Add(1)
		return false
	}
	// Pion invokes the quota callback immediately before allocation creation.
	// Reserve the slot so concurrent requests cannot all pass the same check.
	// Failed creates are released conservatively by the short expiry.
	q.reservations[source] = quotaReservation{userID: userID, expiresAt: now.Add(10 * time.Second)}
	return true
}

func (u *userRateLimiters) get(userID, realm string) *rate.Limiter {
	key := userID + "\x00" + realm
	if existing, ok := u.values.Load(key); ok {
		return existing.(*rate.Limiter)
	}
	created := rate.NewLimiter(u.limit, u.burst)
	actual, _ := u.values.LoadOrStore(key, created)
	return actual.(*rate.Limiter)
}

func (u *userRateLimiters) remove(userID, realm string) {
	u.values.Delete(userID + "\x00" + realm)
}

func (g *relayGenerator) AllocatePacketConn(
	configuration pionturn.AllocateListenerConfig,
) (net.PacketConn, net.Addr, error) {
	connection, address, err := g.RelayAddressGenerator.AllocatePacketConn(configuration)
	if err != nil {
		return nil, nil, err
	}
	return &meteredPacketConn{
		PacketConn: connection, limiter: g.limiters.get(configuration.UserID, configuration.Realm),
		readBytes: g.readBytes, writtenBytes: g.writtenBytes,
		bandwidthDenied: g.bandwidthDenied,
	}, address, nil
}

func (c *meteredPacketConn) ReadFrom(payload []byte) (int, net.Addr, error) {
	for {
		count, address, err := c.PacketConn.ReadFrom(payload)
		if err != nil {
			return count, address, err
		}
		if c.limiter.AllowN(time.Now(), count) {
			c.readBytes.Add(uint64(count))
			return count, address, nil
		}
		c.bandwidthDenied.Add(1)
	}
}

func (c *meteredPacketConn) WriteTo(payload []byte, address net.Addr) (int, error) {
	if !c.limiter.AllowN(time.Now(), len(payload)) {
		c.bandwidthDenied.Add(1)
		return len(payload), nil
	}
	count, err := c.PacketConn.WriteTo(payload, address)
	if count > 0 {
		c.writtenBytes.Add(uint64(count))
	}
	return count, err
}

func (q *quota) created(userID, source string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.reservations, source)
	q.total++
	q.byUser[userID]++
}

func (q *quota) expireReservations(now time.Time) {
	for source, reservation := range q.reservations {
		if !now.Before(reservation.expiresAt) {
			delete(q.reservations, source)
		}
	}
}

func (q *quota) deleted(userID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.total > 0 {
		q.total--
	}
	if q.byUser[userID] <= 1 {
		delete(q.byUser, userID)
	} else {
		q.byUser[userID]--
	}
}
