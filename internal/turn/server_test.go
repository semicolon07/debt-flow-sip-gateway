package turn

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestQuotaAndServerStatsExposeBoundedDenials(t *testing.T) {
	q := &quota{
		total: 1, byUser: map[string]int{"session-1": 1}, reservations: make(map[string]quotaReservation),
		maxTotal: 1, maxByUser: 1,
	}
	if q.allow("session-2", "10.0.0.2:5000", time.Now()) {
		t.Fatal("global TURN quota must reject another allocation")
	}
	server := &Server{quota: q}
	server.authDenied.Add(2)
	server.bandwidthDenied.Add(3)
	server.bytesRead.Add(100)
	server.bytesWritten.Add(200)

	stats := server.Stats()
	if stats.Allocations != 1 || stats.AllocationLimit != 1 || stats.AuthenticationDenials != 2 ||
		stats.QuotaDenials != 1 || stats.BandwidthDenials != 3 ||
		stats.PeerToClientBytes != 100 || stats.ClientToPeerBytes != 200 {
		t.Fatalf("unexpected TURN stats: %+v", stats)
	}
}

func TestQuotaAtomicallyReservesConcurrentAllocationSlot(t *testing.T) {
	now := time.Unix(100, 0)
	q := &quota{
		byUser: make(map[string]int), reservations: make(map[string]quotaReservation),
		maxTotal: 1, maxByUser: 1,
	}
	if !q.allow("session-1", "10.0.0.1:5000", now) {
		t.Fatal("first allocation should reserve the slot")
	}
	if q.allow("session-2", "10.0.0.2:5000", now) {
		t.Fatal("concurrent allocation passed a reserved global slot")
	}
	if !q.allow("session-1", "10.0.0.1:5000", now) {
		t.Fatal("retransmission from the reserved source should remain allowed")
	}
	q.created("session-1", "10.0.0.1:5000")
	if q.total != 1 || len(q.reservations) != 0 {
		t.Fatalf("reservation was not converted to an allocation: %+v", q)
	}
}

func TestQuotaExpiresFailedAllocationReservation(t *testing.T) {
	now := time.Unix(100, 0)
	q := &quota{
		byUser: make(map[string]int), reservations: make(map[string]quotaReservation),
		maxTotal: 1, maxByUser: 1,
	}
	if !q.allow("session-1", "10.0.0.1:5000", now) {
		t.Fatal("first reservation rejected")
	}
	if !q.allow("session-2", "10.0.0.2:5000", now.Add(11*time.Second)) {
		t.Fatal("expired failed-allocation reservation still blocked the slot")
	}
}

func TestMeteredPacketConnCountsForwardedBytesAndDropsOverQuota(t *testing.T) {
	underlying := &fakePacketConn{}
	var readBytes, writtenBytes, denied atomic.Uint64
	connection := &meteredPacketConn{
		PacketConn: underlying, limiter: rate.NewLimiter(1000, 1000),
		readBytes: &readBytes, writtenBytes: &writtenBytes, bandwidthDenied: &denied,
	}
	payload := []byte{1, 2, 3, 4}
	count, err := connection.WriteTo(payload, &net.UDPAddr{})
	if err != nil || count != len(payload) || writtenBytes.Load() != uint64(len(payload)) {
		t.Fatalf("unexpected metered write: count=%d bytes=%d err=%v", count, writtenBytes.Load(), err)
	}

	connection.limiter = rate.NewLimiter(1, 1)
	count, err = connection.WriteTo(payload, &net.UDPAddr{})
	if err != nil || count != len(payload) || underlying.writes.Load() != 1 || denied.Load() != 1 {
		t.Fatalf("over-quota packet was not dropped: count=%d writes=%d denied=%d err=%v",
			count, underlying.writes.Load(), denied.Load(), err)
	}
}

type fakePacketConn struct {
	writes atomic.Uint64
}

func (*fakePacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, nil, errors.New("not implemented")
}

func (c *fakePacketConn) WriteTo(payload []byte, _ net.Addr) (int, error) {
	c.writes.Add(1)
	return len(payload), nil
}

func (*fakePacketConn) Close() error                     { return nil }
func (*fakePacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (*fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (*fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (*fakePacketConn) SetWriteDeadline(time.Time) error { return nil }
