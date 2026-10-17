package call

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

type Direction string

const (
	Outbound Direction = "outbound"
	Inbound  Direction = "inbound"
)

type State string

const (
	StateCreated     State = "created"
	StateAuthorizing State = "authorizing"
	StateDialing     State = "dialing"
	StateTrying      State = "trying"
	StateRinging     State = "ringing"
	StateIncoming    State = "incoming"
	StateAnswering   State = "answering"
	StateConnected   State = "connected"
	StateEnding      State = "ending"
	StateEnded       State = "ended"
)

type EndReason string

const (
	EndLocalHangup      EndReason = "local_hangup"
	EndRemoteHangup     EndReason = "remote_hangup"
	EndRejected         EndReason = "rejected"
	EndCancelled        EndReason = "cancelled"
	EndBusy             EndReason = "busy"
	EndNoAnswer         EndReason = "no_answer"
	EndAuthorization    EndReason = "authorization_failed"
	EndGatewayRestarted EndReason = "gateway_restarted"
	EndLeaseRevoked     EndReason = "lease_revoked"
	EndAPIUnavailable   EndReason = "api_unavailable"
	EndMediaFailed      EndReason = "media_failed"
	EndSIPFailed        EndReason = "sip_failed"
)

type Snapshot struct {
	CallID      string     `json:"callId"`
	Direction   Direction  `json:"direction"`
	State       State      `json:"state"`
	EndReason   EndReason  `json:"endReason,omitempty"`
	CreatedAt   time.Time  `json:"createdAtUtc"`
	ConnectedAt *time.Time `json:"connectedAtUtc,omitempty"`
	EndedAt     *time.Time `json:"endedAtUtc,omitempty"`
	Revision    uint64     `json:"revision"`
}

type Action string

const (
	ActionNone   Action = "none"
	ActionInvite Action = "send_invite"
	ActionCancel Action = "send_cancel"
	ActionACKBYE Action = "send_ack_then_bye"
	ActionBYE    Action = "send_bye"
	ActionAnswer Action = "send_answer"
	ActionReject Action = "send_reject"
)

type Machine struct {
	mu       sync.Mutex
	snapshot Snapshot
}

func NewOutbound(callID string, now time.Time) (*Machine, error) {
	if callID == "" {
		return nil, errors.New("callID is required")
	}
	return &Machine{snapshot: Snapshot{
		CallID: callID, Direction: Outbound, State: StateCreated,
		CreatedAt: now.UTC(), Revision: 1,
	}}, nil
}

func NewInbound(callID string, now time.Time) (*Machine, error) {
	if callID == "" {
		return nil, errors.New("callID is required")
	}
	return &Machine{snapshot: Snapshot{
		CallID: callID, Direction: Inbound, State: StateCreated,
		CreatedAt: now.UTC(), Revision: 1,
	}}, nil
}

func Restore(snapshot Snapshot) (*Machine, error) {
	if err := ValidateSnapshot(snapshot); err != nil {
		return nil, err
	}
	return &Machine{snapshot: snapshot}, nil
}

func ValidateSnapshot(snapshot Snapshot) error {
	if snapshot.CallID == "" || snapshot.Revision == 0 || snapshot.CreatedAt.IsZero() {
		return errors.New("invalid call snapshot")
	}
	if snapshot.Direction != Outbound && snapshot.Direction != Inbound {
		return errors.New("invalid call direction")
	}
	switch snapshot.State {
	case StateCreated, StateAuthorizing, StateDialing, StateTrying, StateRinging, StateIncoming, StateAnswering,
		StateConnected, StateEnding, StateEnded:
	default:
		return errors.New("invalid call state")
	}
	created := snapshot.CreatedAt.UTC()
	if snapshot.ConnectedAt != nil && snapshot.ConnectedAt.UTC().Before(created) {
		return errors.New("call connected before it was created")
	}
	if snapshot.State == StateConnected && snapshot.ConnectedAt == nil {
		return errors.New("connected call has no connected timestamp")
	}
	if snapshot.State == StateEnded {
		if !validEndReason(snapshot.EndReason) || snapshot.EndedAt == nil {
			return errors.New("ended call has no valid terminal reason or timestamp")
		}
		ended := snapshot.EndedAt.UTC()
		if ended.Before(created) || snapshot.ConnectedAt != nil && ended.Before(snapshot.ConnectedAt.UTC()) {
			return errors.New("call ended before an earlier lifecycle timestamp")
		}
	} else if snapshot.EndReason != "" || snapshot.EndedAt != nil {
		return errors.New("active call contains terminal fields")
	}
	return nil
}

func validEndReason(reason EndReason) bool {
	switch reason {
	case EndLocalHangup, EndRemoteHangup, EndRejected, EndCancelled, EndBusy, EndNoAnswer,
		EndAuthorization, EndGatewayRestarted, EndLeaseRevoked, EndAPIUnavailable,
		EndMediaFailed, EndSIPFailed:
		return true
	default:
		return false
	}
}

func (m *Machine) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot
}

func (m *Machine) BeginAuthorization() error {
	return m.transition(StateCreated, StateAuthorizing)
}

func (m *Machine) AuthorizationSucceeded() (Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(StateAuthorizing); err != nil {
		return ActionNone, err
	}
	m.setState(StateDialing)
	return ActionInvite, nil
}

func (m *Machine) MarkRinging() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Direction != Outbound ||
		(m.snapshot.State != StateDialing && m.snapshot.State != StateTrying && m.snapshot.State != StateRinging) {
		return fmt.Errorf("cannot mark %s call ringing from %s", m.snapshot.Direction, m.snapshot.State)
	}
	if m.snapshot.State != StateRinging {
		m.setState(StateRinging)
	}
	return nil
}

func (m *Machine) MarkTrying() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Direction != Outbound ||
		(m.snapshot.State != StateDialing && m.snapshot.State != StateTrying) {
		return fmt.Errorf("cannot mark %s call trying from %s", m.snapshot.Direction, m.snapshot.State)
	}
	if m.snapshot.State != StateTrying {
		m.setState(StateTrying)
	}
	return nil
}

func (m *Machine) MarkIncoming() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Direction != Inbound || m.snapshot.State != StateCreated {
		return fmt.Errorf("cannot mark %s call incoming from %s", m.snapshot.Direction, m.snapshot.State)
	}
	m.setState(StateIncoming)
	return nil
}

func (m *Machine) AnswerInbound() (Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Direction != Inbound || m.snapshot.State != StateIncoming {
		return ActionNone, fmt.Errorf("cannot answer %s call from %s", m.snapshot.Direction, m.snapshot.State)
	}
	m.setState(StateAnswering)
	return ActionAnswer, nil
}

func (m *Machine) RejectInbound(now time.Time) (Action, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Direction != Inbound || m.snapshot.State != StateIncoming {
		return ActionNone, false
	}
	m.end(EndRejected, now)
	return ActionReject, true
}

func (m *Machine) Connected(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.State != StateDialing && m.snapshot.State != StateTrying &&
		m.snapshot.State != StateRinging && m.snapshot.State != StateIncoming && m.snapshot.State != StateAnswering {
		return fmt.Errorf("cannot connect call from %s", m.snapshot.State)
	}
	connected := now.UTC()
	m.snapshot.ConnectedAt = &connected
	m.setState(StateConnected)
	return nil
}

// BeginEnding records the durable, non-terminal transition that must precede a
// user-initiated SIP teardown. CompleteEnding terminalizes the call after the
// SIP side effect has been attempted.
func (m *Machine) BeginEnding() (Action, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.State == StateEnding || m.snapshot.State == StateEnded {
		return ActionNone, false
	}
	action := ActionCancel
	if m.snapshot.Direction == Inbound && m.snapshot.State == StateIncoming {
		action = ActionReject
	} else if m.snapshot.State == StateConnected {
		action = ActionBYE
	}
	m.setState(StateEnding)
	return action, true
}

func (m *Machine) BeginRejectInbound() (Action, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Direction != Inbound || m.snapshot.State != StateIncoming {
		return ActionNone, false
	}
	m.setState(StateEnding)
	return ActionReject, true
}

func (m *Machine) CompleteEnding(reason EndReason, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.State != StateEnding {
		return false
	}
	m.end(reason, now)
	return true
}

func (m *Machine) LocalHangup(now time.Time) (Action, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.State == StateEnded {
		return ActionNone, false
	}
	action := ActionCancel
	if m.snapshot.Direction == Inbound && m.snapshot.State == StateIncoming {
		action = ActionReject
	} else if m.snapshot.State == StateConnected {
		action = ActionBYE
	}
	m.end(EndLocalHangup, now)
	return action, true
}

func (m *Machine) RemoteHangup(reason EndReason, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.State == StateEnded {
		return false
	}
	if reason == "" {
		reason = EndRemoteHangup
	}
	m.end(reason, now)
	return true
}

func (m *Machine) AuthorizationFailed(now time.Time) bool {
	return m.RemoteHangup(EndAuthorization, now)
}

func (m *Machine) LateAnswerAfterEnd() Action {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Direction == Outbound && m.snapshot.State == StateEnded {
		return ActionACKBYE
	}
	return ActionNone
}

func (m *Machine) transition(from, to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(from); err != nil {
		return err
	}
	m.setState(to)
	return nil
}

func (m *Machine) require(state State) error {
	if m.snapshot.State != state {
		return fmt.Errorf("expected call state %s, got %s", state, m.snapshot.State)
	}
	return nil
}

func (m *Machine) setState(state State) {
	m.snapshot.State = state
	m.snapshot.Revision++
}

func (m *Machine) end(reason EndReason, now time.Time) {
	ended := now.UTC()
	m.snapshot.State = StateEnded
	m.snapshot.EndReason = reason
	m.snapshot.EndedAt = &ended
	m.snapshot.Revision++
}
