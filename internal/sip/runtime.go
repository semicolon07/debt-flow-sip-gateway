package sip

import (
	"context"
	"errors"

	"github.com/pion/rtp"
)

type Transport string

const (
	TransportUDP Transport = "udp"
	TransportTCP Transport = "tcp"
)

type Configuration struct {
	Host      string
	Port      uint16
	Transport Transport
	Username  string
	Password  string
	Realm     string
}

type MediaDescription struct {
	IP               string
	Port             uint16
	RTCPIP           string
	RTCPPort         uint16
	AudioPayloadType uint8
	DTMFPayloadType  uint8
	Codec            string
}

type EventType string

const (
	EventRegistering          EventType = "registering"
	EventRegistrationRetrying EventType = "retrying"
	EventRegistered           EventType = "registered"
	EventUnregistered         EventType = "unregistered"
	EventRegistrationFailed   EventType = "registration_failed"
	EventIncoming             EventType = "incoming"
	EventTrying               EventType = "trying"
	EventRinging              EventType = "ringing"
	EventConnected            EventType = "connected"
	EventEnded                EventType = "ended"
	EventFailed               EventType = "failed"
	EventRTP                  EventType = "rtp"
)

type Event struct {
	Type        EventType
	SessionID   string
	CallID      string
	StatusCode  int
	Reason      string
	RemoteSDP   []byte
	RemoteParty string
	Packet      *rtp.Packet
}

type Runtime interface {
	Events() <-chan Event
	Configure(context.Context, string, Configuration) error
	Register(context.Context, string) error
	Unregister(context.Context, string) error
	StartCall(context.Context, string, string, string, []byte) error
	Answer(context.Context, string, string, []byte) error
	Reject(context.Context, string, string, int) error
	Hangup(context.Context, string, string) error
	SendRTP(context.Context, string, string, *rtp.Packet) error
	SendDTMF(context.Context, string, string, byte) error
	CloseSession(context.Context, string) error
	Close() error
}

var ErrSpikeGateNotPassed = errors.New("SIPGo interoperability spike gate has not passed")

// GatedRuntime keeps the production process fail-closed until the Issabel/Asterisk
// interoperability suite has explicitly approved a SIP adapter.
type GatedRuntime struct {
	events chan Event
}

func NewGatedRuntime() *GatedRuntime { return &GatedRuntime{events: make(chan Event)} }

func (r *GatedRuntime) Events() <-chan Event { return r.events }

func (*GatedRuntime) Configure(context.Context, string, Configuration) error {
	return ErrSpikeGateNotPassed
}
func (*GatedRuntime) Register(context.Context, string) error   { return ErrSpikeGateNotPassed }
func (*GatedRuntime) Unregister(context.Context, string) error { return ErrSpikeGateNotPassed }
func (*GatedRuntime) StartCall(context.Context, string, string, string, []byte) error {
	return ErrSpikeGateNotPassed
}
func (*GatedRuntime) Answer(context.Context, string, string, []byte) error {
	return ErrSpikeGateNotPassed
}
func (*GatedRuntime) Reject(context.Context, string, string, int) error { return ErrSpikeGateNotPassed }
func (*GatedRuntime) Hangup(context.Context, string, string) error      { return ErrSpikeGateNotPassed }
func (*GatedRuntime) SendRTP(context.Context, string, string, *rtp.Packet) error {
	return ErrSpikeGateNotPassed
}
func (*GatedRuntime) SendDTMF(context.Context, string, string, byte) error {
	return ErrSpikeGateNotPassed
}
func (*GatedRuntime) CloseSession(context.Context, string) error { return nil }
func (r *GatedRuntime) Close() error                             { close(r.events); return nil }
