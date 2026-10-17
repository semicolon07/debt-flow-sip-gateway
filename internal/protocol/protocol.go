package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

const Version = 1

type Kind string

const (
	KindCommand Kind = "command"
	KindEvent   Kind = "event"
	KindResult  Kind = "result"
	KindError   Kind = "error"
)

type Envelope struct {
	ProtocolVersion int             `json:"protocolVersion"`
	Kind            Kind            `json:"kind"`
	MessageID       string          `json:"messageId"`
	SentAtUTC       time.Time       `json:"sentAtUtc"`
	Type            string          `json:"type"`
	Payload         json.RawMessage `json:"payload"`
}

type Authenticate struct {
	CollectionID              string `json:"collectionId"`
	SessionCapability         string `json:"sessionCapability"`
	PortalVersion             string `json:"portalVersion"`
	SupportedProtocolVersions []int  `json:"supportedProtocolVersions"`
}

type SessionRenew struct {
	CommandID         string `json:"commandId"`
	SessionCapability string `json:"sessionCapability"`
}

type CommandIdentity struct {
	CommandID string `json:"commandId"`
}

type CallCommand struct {
	CommandID string `json:"commandId"`
	CallID    string `json:"callId"`
}

type StartCall struct {
	CommandID         string `json:"commandId"`
	CallID            string `json:"callId"`
	CallContextID     string `json:"callContextId"`
	GatewayCapability string `json:"gatewayCapability"`
}

type ICECandidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex,omitempty"`
}

type WebRTCOffer struct {
	SDP string `json:"sdp"`
}

type EventsACK struct {
	Sequence uint64 `json:"sequence"`
}

type DTMF struct {
	CommandID string `json:"commandId"`
	CallID    string `json:"callId"`
	Digit     string `json:"digit"`
}

var commandTypes = map[string]struct{}{
	"session.authenticate": {}, "session.renew": {}, "state.get": {}, "events.ack": {},
	"registration.start": {}, "registration.stop": {},
	"webrtc.offer": {}, "webrtc.ice_candidate": {},
	"call.start": {}, "call.answer": {}, "call.reject": {},
	"call.hangup": {}, "call.dtmf": {},
}

func Decode(data []byte) (Envelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Envelope{}, errors.New("decode envelope: trailing JSON value")
		}
		return Envelope{}, fmt.Errorf("decode envelope trailing data: %w", err)
	}
	if envelope.ProtocolVersion != Version {
		return Envelope{}, fmt.Errorf("unsupported protocol version %d", envelope.ProtocolVersion)
	}
	if envelope.Kind != KindCommand {
		return Envelope{}, fmt.Errorf("client kind must be %q", KindCommand)
	}
	if _, ok := commandTypes[envelope.Type]; !ok {
		return Envelope{}, fmt.Errorf("unsupported command type %q", envelope.Type)
	}
	if _, err := uuid.Parse(envelope.MessageID); err != nil {
		return Envelope{}, errors.New("messageId must be a UUID")
	}
	if envelope.SentAtUTC.IsZero() {
		return Envelope{}, errors.New("sentAtUtc is required")
	}
	if len(envelope.Payload) == 0 || bytes.Equal(envelope.Payload, []byte("null")) {
		return Envelope{}, errors.New("payload is required")
	}
	return envelope, nil
}

func DecodePayload[T any](envelope Envelope) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s payload: %w", envelope.Type, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, fmt.Errorf("decode %s payload: trailing JSON value", envelope.Type)
		}
		return value, fmt.Errorf("decode %s payload trailing data: %w", envelope.Type, err)
	}
	return value, nil
}

func ValidateAuthenticate(value Authenticate) error {
	if value.CollectionID == "" || value.SessionCapability == "" || value.PortalVersion == "" {
		return errors.New("collectionId, sessionCapability and portalVersion are required")
	}
	for _, version := range value.SupportedProtocolVersions {
		if version == Version {
			return nil
		}
	}
	return errors.New("client does not support protocol version 1")
}

func ValidateStartCall(value StartCall) error {
	if err := ValidateCommandID(value.CommandID); err != nil {
		return errors.New("commandId must be a UUID")
	}
	if _, err := uuid.Parse(value.CallID); err != nil {
		return errors.New("callId must be a UUID")
	}
	if value.CallContextID == "" || value.GatewayCapability == "" {
		return errors.New("callContextId and gatewayCapability are required")
	}
	return nil
}

func ValidateSessionRenew(value SessionRenew) error {
	if err := ValidateCommandID(value.CommandID); err != nil {
		return errors.New("commandId must be a UUID")
	}
	if value.SessionCapability == "" {
		return errors.New("sessionCapability is required")
	}
	return nil
}

func ValidateCommandID(commandID string) error {
	_, err := uuid.Parse(commandID)
	return err
}

func ValidateCallCommand(value CallCommand) error {
	if err := ValidateCommandID(value.CommandID); err != nil {
		return errors.New("commandId must be a UUID")
	}
	if _, err := uuid.Parse(value.CallID); err != nil {
		return errors.New("callId must be a UUID")
	}
	return nil
}

func ValidateDTMF(value DTMF) error {
	if err := ValidateCommandID(value.CommandID); err != nil {
		return errors.New("commandId must be a UUID")
	}
	if _, err := uuid.Parse(value.CallID); err != nil {
		return errors.New("callId must be a UUID")
	}
	if len(value.Digit) != 1 || !bytes.ContainsRune([]byte("0123456789*#ABCD"), rune(value.Digit[0])) {
		return errors.New("digit must be one RFC 4733 DTMF symbol")
	}
	return nil
}

func NewEvent(eventType string, payload any, now time.Time) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode %s payload: %w", eventType, err)
	}
	return Envelope{
		ProtocolVersion: Version,
		Kind:            KindEvent,
		MessageID:       uuid.NewString(),
		SentAtUTC:       now.UTC(),
		Type:            eventType,
		Payload:         raw,
	}, nil
}
