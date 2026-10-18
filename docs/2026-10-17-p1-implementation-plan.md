# Debt Flow SIP Gateway — Confirmed P1 Implementation Plan

วันที่: 17/10/2026

สถานะ: **CLOSED — functional scope accepted 18/10/2026 / Production release qualification pending**

การปิด P1 หมายถึงขอบเขตการพัฒนา, automated verification และ reported functional flow ที่ระบุในเอกสารนี้
ได้รับการส่งมอบและผู้ใช้ยืนยัน corrective build บนระบบที่ deploy แล้วเมื่อ 18/10/2026
ไม่ใช่การรับรองผล pilot หรือ production capacity บน Windows Server/PBX จริง รายการ external evidence
ที่ยังต้องเก็บให้ติดตามต่อใน `pilot-acceptance-matrix.md` และห้ามข้ามก่อนเปิดใช้กับผู้ใช้จริง

Repository: `sip-gateway`

ภาษาและ runtime: Go, Windows Server 2022 x64

เจ้าของเอกสาร: Debt Flow engineering

## 1. Executive summary

P1 จะสร้าง SIP Gateway กลางเพื่อให้ผู้ใช้ Debt Flow Portal โทรผ่าน Microsoft Edge หรือ
Google Chrome ได้โดยไม่ต้องติดตั้ง SIP Agent บนเครื่อง client โดย browser ใช้ WSS สำหรับ
control/signaling และ WebRTC สำหรับเสียง ส่วน gateway เป็นผู้ terminate DTLS-SRTP, ทำ SIP
registration/call control และ bridge media ไปยัง Issabel/Asterisk ผ่าน RTP

P1 เป็น single-node deployment บน Windows Server 2022 และส่งมอบเป็น executable เดียวที่รันเป็น
Windows Service โดยฝัง TURN server ไว้ใน process เดียวกัน ระบบรองรับทั้ง inbound และ outbound,
หนึ่ง active call ต่อ user, codec PCMU/PCMA passthrough และ RFC 4733 DTMF โดยไม่มี transcoding

เป้าหมาย validation floor คือ 250 registered sessions และ 200 concurrent calls บน private/VPN
network งานนี้เป็น production-oriented MVP ที่มี durability, authentication, observability และ
rollback path ครบ แต่ยังไม่รวม HA, multi-node, advanced telephony และ public-internet deployment

## 2. Decisions ที่ยืนยันแล้ว

| หัวข้อ | ข้อสรุป P1 |
|---|---|
| ภาษา | Go |
| OS | Windows Server 2022 x64 |
| Topology | Single node |
| Browser | Edge และ Chrome |
| Client access | Private/VPN เท่านั้น |
| Gateway signaling | WSS ผ่าน IIS reverse proxy |
| Browser media | WebRTC, DTLS-SRTP, ICE |
| TURN | Pion TURN v5 ฝังใน executable เดียว |
| SIP stack | SIPGo candidate; ต้องผ่าน interoperability spike ก่อนเริ่ม implementation หลัก |
| PBX acceptance target | Issabel/Asterisk |
| SIP transport | UDP และ TCP |
| Call direction | Inbound และ outbound |
| Codec | PCMU/PCMA passthrough |
| DTMF | RFC 4733 `telephone-event` |
| Transcoding | ไม่มีใน P1 |
| Capacity floor | 250 registered sessions / 200 concurrent calls |
| Persistent state | SQLite WAL |
| At-rest protection | Windows DPAPI + NTFS ACL |
| Gateway → API | ไม่มี direct connection; browser relay encrypted capabilities |
| Event ingest | Browser relay ผ่าน `PhoneEventRelayService` และ Web API เดิม |
| Rollout selector | `runtimeMode` ต่อ Collection |
| Legacy rollback | `sip-windows-agent` ยังอยู่และเป็น explicit rollback path |
| Automatic fallback | ไม่มี |

## 3. Scope และ acceptance goals

### 3.1 In scope

- ติดตั้ง `sip-gateway.exe` เป็น Windows Service ภายใต้ service identity เฉพาะ
- IIS terminate TLS/WSS และ reverse proxy `/gateway/v1` ไปยัง gateway บน loopback
- one-time encrypted capability, WSS authentication, browser-relayed lease และ bounded revocation
- WebRTC offer/answer, trickle ICE, direct ICE และ TURN fallback
- embedded TURN ผ่าน UDP/TCP 3478 และ bounded relay port range
- SIP REGISTER digest authentication ผ่าน UDP/TCP
- outbound: dial, ringing, answer, cancel, busy, no-answer และ hangup
- inbound: ringing, answer, reject, remote cancel และ hangup
- PCMU/PCMA RTP/RTCP bridge โดยไม่ decode/transcode
- RFC 4733 DTMF
- browser audio parity: mute, gain, device selection, input/output test และ device switching
- authoritative snapshot, event ACK/replay และ durable command/event journal
- explicit handling สำหรับ logout, revoke, disabled user, security change และ Web API outage
- health, readiness, metrics และ sanitized structured logs
- installer/runbook/configuration สำหรับ Windows Server 2022
- automated tests, SIPp interoperability และ browser/PBX acceptance matrix

### 3.2 Out of scope

- Opus ↔ G.711 หรือ codec transcoding อื่น
- SIP TLS หรือ SRTP ฝั่ง PBX
- TURN/TLS บน port 443
- call recording, hold, transfer, conference, whisper, monitor และ barge-in
- video, screen sharing และ data channels
- multiple simultaneous calls ต่อ user
- HA, active-active, multi-node ownership และ node failover
- media reattachment หลัง tab reload
- rolling/zero-downtime binary upgrade ระหว่าง active call
- public-internet exposure ของ Gateway, Web API หรือ PBX
- automatic fallback ระหว่าง gateway กับ desktop agent

## 4. Target architecture

```text
Debt Flow Portal (Edge / Chrome over private VPN)
  │
  ├── HTTPS ─────────────────────────────► Debt Flow Web API
  │                                        ├─ authorize user/Collection
  │                                        ├─ issue encrypted session capability
  │                                        └─ issue encrypted call capability with canonical destination
  │
  ├── WSS :443 /gateway/v1 ──────────────► IIS
  │                                          │ reverse proxy
  │                                          ▼
  │                                     localhost:8080
  │                                     sip-gateway.exe
  │                                        ├─ session + call state machines
  │                                        ├─ Pion WebRTC v4
  │                                        ├─ SIPGo adapter
  │                                        ├─ RTP bridge
  │                                        ├─ Pion TURN v5
  │                                        ├─ SQLite WAL + DPAPI
  │                                        └─ health/metrics/logging
  │
  └── ICE / DTLS-SRTP ───────────────────► Gateway media or embedded TURN

sip-gateway.exe
  ├── no outbound Web API connection
  ├── local capability decrypt/verify ────► provisioned key ring
  ├── SIP UDP/TCP ────────────────────────► Issabel/Asterisk
  └── RTP/RTCP UDP ───────────────────────► Issabel/Asterisk
```

### 4.1 Network and DNS

- `sip-gateway.uniga.app` ใช้ private/split DNS ชี้ไปยัง static VPN IP ของ Windows Server
- IIS ใช้ public ACME certificate ที่ออกด้วย DNS-01 challenge เพื่อไม่ต้องติดตั้ง private root CA
  บนเครื่อง client
- Browser, Gateway, Web API private ingress และ PBX ติดต่อกันผ่าน private/VPN network เท่านั้น
- Gateway HTTP listener ฟังเฉพาะ `127.0.0.1:8080`; ห้าม expose ตรงออก network
- Production ใช้ `http.allowedOrigins` เป็น exact HTTPS allowlist หลายค่า; รองรับ explicit `*`
  เมื่อยอมรับความเสี่ยง และ production preflight จะ flag wildcard

### 4.2 Proposed ports

| Port/range | Protocol | Owner | Purpose |
|---|---|---|---|
| 443 | TCP/TLS | IIS | Portal WSS `/gateway/v1` |
| 8080 | TCP loopback | Gateway | IIS upstream HTTP/WebSocket |
| 5060 | UDP/TCP | Gateway | SIP signaling ไป/กลับ PBX |
| 20000–21999 | UDP | Gateway | PBX-side RTP/RTCP |
| 22000 | UDP | Pion ICE UDP mux | Browser direct ICE media |
| 3478 | UDP/TCP | Embedded TURN | TURN listener |
| 30000–30999 | UDP | Embedded TURN | TURN relay allocation |
| 9090 | TCP loopback | Gateway | Metrics endpoint |

Port และ firewall rule ต้องเป็น configuration ที่ validate ตอน startup ค่าตามตารางเป็น P1 default
และต้องยืนยันว่าไม่ชนกับ Asterisk RTP range ของ environment จริง

### 4.3 Process and service model

- binary: `C:\Program Files\DebtFlow\SipGateway\sip-gateway.exe`
- configuration: `C:\ProgramData\DebtFlow\SipGateway\config.yaml`
- SQLite: `C:\ProgramData\DebtFlow\SipGateway\data\gateway.db`
- logs: `C:\ProgramData\DebtFlow\SipGateway\logs`
- Windows Service identity: `NT SERVICE\DebtFlowSipGateway`
- Startup type: Automatic (Delayed Start)
- Service Recovery: restart on unexpected failure ด้วย bounded backoff
- Graceful shutdown budget: 15 วินาที; หยุดรับ session/call ใหม่และพยายามจบ state ให้ deterministic
- NTFS ACL ให้ service identity และ local administrators เท่านั้นอ่าน configuration/data/log ที่ sensitive

## 5. Component design

### 5.1 Repository layout

```text
cmd/sip-gateway/          process entry point and Windows Service host
internal/gateway/         lifecycle, dependency wiring, health/readiness
internal/protocol/        WSS envelopes, validation, versions, errors
internal/call/            session/registration/call state machines
internal/webrtc/          Pion PeerConnection, ICE, tracks and RTCP
internal/sip/             SIPGo adapter and Issabel/Asterisk quirk profile
internal/media/           RTP/RTCP mapping and passthrough bridge
internal/turn/            embedded Pion TURN lifecycle and credentials
internal/capability/      local encrypted session/call capability verification
internal/store/           SQLite schema, journal, outbox and retention
internal/security/        redaction, quotas, rate limits and authorization
internal/observability/   slog, Prometheus metrics and diagnostics
```

### 5.2 Proposed dependencies

| Area | Dependency / approach |
|---|---|
| WebRTC | `github.com/pion/webrtc/v4` |
| TURN | `github.com/pion/turn/v5` |
| WebSocket | `github.com/coder/websocket` |
| SIP | SIPGo หลังผ่าน spike gate |
| SQLite | `modernc.org/sqlite` เพื่อคง pure-Go single executable |
| Windows APIs | `golang.org/x/sys/windows` |
| Logging | standard library `log/slog` |
| Metrics | Prometheus Go client |

Dependency versions ต้อง pin ใน `go.mod` และมี SBOM/license/security review ก่อน release P1

### 5.3 PBX adapter boundary

SIP behavior ต้องอยู่หลัง interface ที่แยกจาก call state machine เพื่อรองรับ provider/firmware quirk
ในอนาคต P1 มี `AsteriskProfile` สำหรับ Issabel/Asterisk และห้ามกระจาย PBX-specific workaround
เข้า protocol, media หรือ session layer

## 6. Rollout model and runtime selection

เพิ่ม `runtimeMode` ใน `SipProviderConfiguration` และ Portal Collection descriptor:

```text
desktop_agent   ค่าเริ่มต้นและ legacy path
gateway         ใช้ SIP Gateway
```

- การแก้ `runtimeMode` เป็น admin-only และต้องมี audit trail
- Portal เลือก runtime จาก authoritative Collection configuration
- ห้าม runtime เดียวกัน register SIP account พร้อมกันโดยไม่ตั้งใจ
- ไม่มี automatic fallback เมื่อ gateway error เพราะอาจทำให้ duplicate registration/call
- rollback ทำโดย admin เปลี่ยน Collection กลับเป็น `desktop_agent` หลังยืนยันว่า gateway session
  และ active call สิ้นสุดแล้ว

## 7. Gateway authentication and capability contracts

### 7.1 Portal session capability endpoint

```http
POST /v1/portal/collections/{collectionId}/phone-gateway/sessions
```

Request:

```json
{
  "agentInstanceId": "uuid",
  "agentSessionId": "uuid",
  "protocolVersion": 1,
  "portalVersion": "string"
}
```

Response:

```json
{
  "gatewayUrl": "wss://sip-gateway.uniga.app/gateway/v1",
  "sessionCapability": "dfcap1.opaque-encrypted-token",
  "expiresAtUtc": "2026-10-17T08:01:00Z",
  "leaseExpiresAtUtc": "2026-10-17T08:02:00Z",
  "protocolVersion": 1
}
```

Capability requirements:

- `dfcap1` เป็น AES-256-GCM authenticated-encryption envelope พร้อม `keyId`
- key ring ถูก provision ล่วงหน้าให้ Web API และ Gateway; Gateway ไม่เรียก Web API เพื่อขอ key
- session capability TTL 60 วินาที, ใช้เริ่ม/ต่ออายุ lease 120 วินาที และ capability ID ใช้ได้ครั้งเดียว
- bind gateway ID, issuer/audience, authenticated user session, Collection/binding,
  agent instance/session และ protocol version
- payload ภายในมี SIP bootstrap แต่ browser เปิดอ่านหรือแก้ไขไม่ได้
- ห้ามใส่ capability ใน URL, query string, log, metric หรือ durable browser storage
- Gateway ตรวจ issuer, audience, gateway ID, issued/not-before/expiry และ exact binding แบบ local

### 7.2 Call capability

`POST /v1/portal/collections/{collectionId}/phone-agent/call-contexts` คืน `gatewayCapability`
เพิ่มเติมเมื่อ request มี `gatewaySessionId` และ Collection ใช้ `runtimeMode=gateway`

- capability ผูก `sessionId`, authenticated Portal `userSessionId`, Collection/binding, `callId`,
  `callContextId` และ canonical destination
- TTL สูงสุด 60 วินาทีและใช้ได้ครั้งเดียว
- `call.start` ต้องแนบ capability; Gateway decrypt/verify locally ก่อน SIP INVITE
- Gateway ห้ามเชื่อ raw destination จาก browser และไม่เรียก Web API เพื่อ authorize call

### 7.3 Capability key lifecycle

- production Gateway เก็บ key ring ในไฟล์ที่เข้ารหัสด้วย Windows DPAPI Local Machine และ NTFS ACL
- Web API โหลด key เดียวกันจาก secret manager/configuration provider; ห้าม commit key ลง repository
- token ระบุ `keyId`; deploy key ใหม่ให้ verifier ก่อนเปลี่ยน active issuer key
- key เก่า/ใหม่ overlap อย่างน้อย 120 วินาทีบวก clock-skew/rollout safety;
  สำหรับ production แนะนำ operational overlap 7 วันเพื่อให้ rollback ได้ปลอดภัย
- หลังไม่มี capability ของ key เก่าเหลือจึงนำ key เก่าออก
- provision/rotate operation ต้องมี audit และห้าม log key, capability หรือ plaintext payload

### 7.4 Session lease and authorization failure policy

- browser ขอ session capability ใหม่จาก Web API ทุก 45 วินาทีแล้วส่ง `session.renew` ทาง WSS
- Gateway ต่อ leaseเป็น 120 วินาทีหลัง verify capability ใหม่สำเร็จ
- explicit logout/tab shutdown ปิด WSS, hangup และ unregister
- server-side revoke, disabled user หรือ security-sensitive change หยุดออก capability ใหม่และมีผลอย่างช้า
  เมื่อ lease 120 วินาทีหมดอายุ
- Web API outage ทำให้ browser ขอ session/call capability ใหม่ไม่ได้; Gateway block call ใหม่ที่ไม่มี
  valid call capability และ terminate/unregister เมื่อ lease หมด
- consumed capability IDs persist ข้าม restart; session/lease ใน memory ไม่ถูก restore และทุก browser
  ต้อง authenticate ใหม่หลัง restart (fail-closed)
- SIP Gateway ไม่มี DNS route, credential หรือ HTTP client สำหรับ Web API

## 8. WSS protocol V1

### 8.1 Connection rules

- URL คงที่ `wss://sip-gateway.uniga.app/gateway/v1` โดยไม่มี credential/query parameter
- IIS ส่ง WebSocket ไปยัง `127.0.0.1:8080`
- ตรวจ Origin กับ exact multi-origin allowlist หรือ explicit wildcard policy
- client ต้องส่ง `session.authenticate` เป็นข้อความแรกภายใน 5 วินาที
- maximum message size 64 KiB
- rate limit 60 messages ต่อ rolling 10 วินาทีต่อ session
- ping/heartbeat interval 15 วินาที และ timeout 45 วินาที
- writer queue ต้อง bounded; slow client ต้องถูก disconnect ด้วย explicit reason
- หลัง authenticate/reconnect ส่งข้อมูลตามลำดับ
  `welcome → durable replay → authoritative snapshot → live` เพื่อให้ event ที่ค้างถูกส่งครบ
  แต่ snapshot ล่าสุดเป็นผู้ตัดสิน volatile registration/call/media state และ replay เก่าไม่ทำให้ UI ถอยหลัง

### 8.2 Envelope

```json
{
  "protocolVersion": 1,
  "kind": "command",
  "messageId": "uuid",
  "sentAtUtc": "2026-10-17T08:00:00Z",
  "type": "call.start",
  "payload": {}
}
```

- properties ใช้ camelCase
- reject unsupported protocol version, invalid type และ malformed payload
- `messageId`, `commandId`, `callId`, `sessionId` ใช้ UUID/opaque identifier ตาม schema
- side-effect command ทุกคำสั่งต้องมี durable deduplication outcome
- duplicate `commandId` และ payload เดิมคืนผลเดิม; payload ต่างกันคืน
  `command_duplicate_conflict`
- command error ที่สัมพันธ์กับคำสั่งต้องคืน `commandId` เพื่อไม่ reject command อื่นที่กำลังรอพร้อมกัน

### 8.3 Authenticate command

```json
{
  "type": "session.authenticate",
  "payload": {
    "collectionId": "opaque-id",
    "sessionCapability": "dfcap1.opaque-encrypted-token",
    "portalVersion": "string",
    "supportedProtocolVersions": [1]
  }
}
```

Gateway decrypt/verify capability ใน process และ consume capability ID ก่อนสร้าง authenticated session

### 8.4 Commands

| Command | Purpose |
|---|---|
| `session.authenticate` | verify one-time session capability และเปิด session |
| `session.renew` | browser relay capability ใหม่เพื่อต่อ lease |
| `state.get` | ขอ authoritative snapshot |
| `events.ack` | ACK durable event ถึง sequence ที่ Portal/Web API commit แล้ว; production client ส่ง `commandId` และรอ correlated `command_result` หลัง SQLite commit; result อาจเลื่อน cursor ผ่าน gap ของ user session อื่นโดยไม่ ACK ข้อมูลข้ามผู้ใช้ |
| `registration.start` | โหลด bootstrap และ register PBX |
| `registration.stop` | unregister เมื่อไม่มี active call |
| `webrtc.offer` | browser ส่ง SDP offer |
| `webrtc.ice_candidate` | browser ส่ง trickle ICE candidate |
| `call.start` | เริ่ม outbound call ด้วย `callContextId` |
| `call.answer` | รับ inbound call |
| `call.reject` | ปฏิเสธ inbound call |
| `call.hangup` | CANCEL หรือ BYE ตาม call state |
| `call.dtmf` | ส่ง RFC 4733 digit โดยไม่ log digit |

`call.start` payload มี `commandId`, `callId`, `callContextId` และ opaque
`gatewayCapability`; Gateway verify exact binding เพื่อรับ canonical destination ก่อนส่ง SIP INVITE

### 8.5 Events

- `session.welcome` พร้อม authoritative `acknowledgedThrough` สำหรับ reconcile หลัง reconnect/ACK response loss
- `gateway.snapshot`
- `registration.state_changed`
- `webrtc.answer`
- `webrtc.ice_candidate`
- `call.created`
- `call.state_changed`
- `call.ended`
- `command_result`
- `error`

heartbeat ใช้ WebSocket control `ping`/`pong` ที่ transport layer จึงไม่มี JSON
`session.pong` envelope ซ้ำอีกชั้น

ทุก event ที่มีผลต่อ business state มี monotonic sequence ตาม persistent `agentInstanceId` และ persist
ใน session outbox ก่อนส่ง เพื่อให้ตรงกับ Web API idempotency key `(agentInstanceId, sequence)` แม้ browser
เปิด gateway session ใหม่ `SipGatewayAdapter` ใน Portal จะแปลง gateway identity เป็น DTO
`agentInstance`/`agentSession` เดิมเพื่อ reuse `PhoneEventRelayService`
event ที่ replay หลัง ACK delivery ไม่แน่นอนต้องถูก ACK ซ้ำโดยไม่ ingest business event ซ้ำ

## 9. Browser session and audio behavior

### 9.1 Session ownership

- หนึ่ง authenticated runtime session ต่อ user/Collection
- WSS disconnect ให้ reconnect grace 60 วินาที; active call ไม่ถูกตัดทันที
- session ใหม่ takeover session เก่าได้เฉพาะเมื่อไม่มี active call
- หาก session เก่ามี active call ให้ reject takeover เพื่อไม่แย่ง media/control ownership
- tab reload ไม่รับประกัน media reattachment; active call จบตาม policy และส่ง terminal event ครั้งเดียว
- inbound call ที่ไม่มี healthy authenticated Portal session ให้ Gateway ตอบ SIP `480 Temporarily Unavailable`

### 9.2 Browser media implementation

- ใช้ persistent `RTCPeerConnection` ต่อ authenticated browser session
- browser เป็น offerer; Gateway เป็น answerer
- microphone track ส่งด้วย `replaceTrack` เมื่อเปลี่ยน input device โดยไม่สร้าง SIP call ใหม่
- ใช้ Web Audio `GainNode` สำหรับ input/output gain และ mute behavior ที่ต้องการ parity
- output device ใช้ `HTMLMediaElement.setSinkId` เมื่อ browser/policy รองรับ
- มี input level/test และ output test sound ใน Portal
- device label แสดงหลังได้รับ media permission เท่านั้น
- device preference เป็น browser-side preference; Gateway ไม่ควบคุม Windows audio device

## 10. SIP, call state and media

### 10.1 SIPGo spike gate

ก่อนสร้าง production modules ต้องทำ isolated spike กับ Issabel/Asterisk จริงและ SIPp โดยทดสอบ:

- REGISTER + digest authentication, refresh และ unregister
- UDP และ TCP transport
- outbound INVITE, provisional response, CANCEL, ACK, BYE และ failure mapping
- inbound INVITE, answer, reject, remote CANCEL และ BYE
- retransmission, duplicate request/response และ timeout
- late 200 OK หลัง local cancel ซึ่งต้อง ACK แล้ว BYE
- simultaneous local/remote hangup
- malformed header/body, unsupported SDP และ transport disconnect

ถ้า SIPGo ไม่ผ่าน gate ให้หยุด milestone และทบทวน dependency/architecture ก่อนดำเนินงานต่อ
ห้ามแก้ด้วย workaround ที่กระจายไปทั้ง codebase

### 10.2 Normalized states

```text
registration: idle → registering → registered → retrying/failed → unregistering → idle
outbound:     created → authorizing → dialing → ringing → connected → ended
inbound:      created → ringing → answering → connected → ended
```

Rules:

- หนึ่ง active call ต่อ user session
- call ทุกสายมี `call.created` และ `call.ended` อย่างละไม่เกินหนึ่งครั้ง
- terminal cause เป็น normalized reason พร้อม sanitized provider code
- command state persist เป็น `received/executing/completed/failed`
- state transition ใช้ serialized owner ต่อ call/dialog เพื่อควบคุม race
- outbound authorization fail ห้ามสร้าง SIP INVITE
- local cancel ระหว่าง provisional state ส่ง CANCEL; connected call ส่ง BYE
- late answer หลัง cancel ต้อง ACK แล้ว BYE และห้ามเปลี่ยนกลับเป็น connected
- simultaneous hangup ต้อง converge ไป terminal state เดียว
- registration lost ระหว่าง active call ไม่ลบ call state; policy ของ dialog/media เป็นผู้ตัดสิน outcome

### 10.3 Media path

- negotiate เฉพาะ PCMU (payload type 0), PCMA (payload type 8) และ negotiated
  `telephone-event`
- forward encoded G.711 payload โดยไม่ decode/transcode
- rewrite SSRC, sequence number, timestamp และ payload type ผ่าน explicit media mapping
- bridge RTCP ที่จำเป็นและเก็บ packet/jitter/loss metrics โดยไม่เก็บ media content
- reject SDP ที่ไม่มี codec intersection ด้วย deterministic error
- RTP source validation และ symmetric RTP behavior ต้องอยู่ใน PBX profile/configuration
- DTMF ใช้ RFC 4733; ห้าม log digit หรือ sequence ของ digit

### 10.4 Embedded TURN

- ใช้ Pion TURN v5 ใน process เดียวกับ Gateway แต่แยก lifecycle/interface ให้ย้ายออกเป็น service แยกได้ภายหลัง
- ฟัง UDP/TCP 3478; P1 ไม่มี TURN/TLS 443
- relay allocation ใช้ UDP 30000–30999
- credential เป็น short-lived/session-scoped และ derive/issue หลัง Gateway authenticate แล้ว
- จำกัด allocation, bandwidth, lifetime และ per-session quota
- reject unauthenticated allocation และ expose allocation/denial metrics
- firewall/NAT mapping ต้องประกาศ advertised IP ที่ browser ผ่าน VPN เข้าถึงได้

## 11. Persistence and recovery

### 11.1 SQLite WAL

Gateway ใช้ SQLite WAL สำหรับ:

- capability key metadata และ consumed capability IDs
- command/event/call journal ที่จำเป็นต่อ safe recovery; session/lease เป็น in-memory และต้อง
  authenticate ใหม่หลัง restart
- command journal และ deduplication result
- event outbox และ ACK watermark
- registration/call state journal
- active-call terminalization record

ข้อมูล secret เช่น SIP password/bootstrap material ที่จำเป็นต้อง persist ต้องเข้ารหัสด้วย Windows
DPAPI ภายใต้ Local Machine context และป้องกันเพิ่มด้วย NTFS ACL ห้าม persist raw capability, DTMF,
SDP, SIP Authorization header หรือ media content

### 11.2 Restart behavior

- process startup ตรวจ schema/integrity และ replay journal ก่อนเปิด readiness
- call ที่ active ตอน process crash/restart ไม่สามารถ media-resume ใน P1
- recovery ต้องสร้าง terminal event `gateway_restarted` เพียงครั้งเดียวต่อ affected call
- durable outbox replay ต่อจนได้ ACK โดยรักษา sequence/order
- duplicate command หลัง restart ต้องได้ recorded outcome หรือ deterministic in-progress recovery result
- ห้าม register/dial จน capability key พร้อมและ browser authenticate session ใหม่สำเร็จ

### 11.3 Bounds and retention

- pending events สูงสุด 10,000 รายการต่อ runtime session
- pending events สูงสุด 100,000 รายการรวม หรือ DB+WAL 1 GiB แล้วแต่ว่าเงื่อนไขใดถึงก่อน
- 80%: warning/alert
- 90%: block call ใหม่ แต่สงวนพื้นที่สำหรับ terminal events และ cleanup
- ACKed events และ completed command records เก็บ 30 วันแล้ว purge เป็น batch
- vacuum/checkpoint ต้องไม่ block active media/call path

## 12. Observability and security controls

### 12.1 Endpoints

- liveness: process/event loops ยังทำงาน
- readiness: database, capability key, listeners และ PBX prerequisites พร้อม
- metrics: loopback `127.0.0.1:9090` และให้ collector ที่ได้รับอนุญาต scrape

### 12.2 Required metrics

- authenticated/connected sessions
- registered/failed registrations
- calls by state/direction/outcome
- call setup latency
- active PeerConnections and ICE route type
- RTP packets, loss, jitter and bridge errors
- TURN allocations, denial, bytes and quota use
- WSS queue depth/disconnect reason
- capability verification/error, lease refresh/expiry and disconnect
- SQLite size/WAL size/outbox depth/checkpoint failures
- goroutine count, memory, CPU and open socket count

### 12.3 Resource and sizing model

- capacity ขึ้นกับจำนวน concurrent media calls มากกว่าจำนวน idle/registered sessions
- CPU หลักอยู่ที่ ICE/DTLS/SRTP ของ PeerConnection และ TURN relay; SIP signaling/SQLite ใช้น้อยกว่า
- RTP bridge ทำ PCMU/PCMA passthrough และ rewrite header โดยไม่ transcode จึงหลีกเลี่ยง CPU cost สูง
- memory หลักคือ PeerConnection, ICE/DTLS/RTP buffers, bounded writer queues และ unacknowledged outbox
- G.711 planning estimate อยู่ราว 80–100 kbps ต่อทิศทางต่อสายเมื่อรวม RTP/UDP/IP overhead;
  TURN บนเครื่องเดียวกันเพิ่ม relay traffic ตาม network hops
- load test บน Windows Server ต้องรายงาน CPU/call, working set/session, GC pause, packet loss, jitter,
  TURN bytes/allocation, socket/port usage และ recovery กลับ baseline หลังจบ test
- ห้ามเพิ่ม Opus↔G.711 transcoding ใน P1; ถ้าต้องเพิ่มต้อง benchmark และตั้ง capacity limit ใหม่

### 12.4 Logging and redaction

- JSON structured logging ผ่าน `slog`
- correlation fields: gateway instance, opaque session, Collection, call และ command identifiers
- redact SIP password, digest response, capability, encryption key, private key, full certificate secret,
  SDP, ICE credential, TURN credential, DTMF และ raw SIP message
- หมายเลขโทรศัพท์ต้อง mask/hash ตาม policy; ห้าม log full destination
- diagnostic bundle ต้องใช้ allowlist และผ่าน automated secret scan
- admin/runtimeMode/capability-key/revoke operations ต้องมี audit event

## 13. Implementation phases

### Phase 0 — Foundation and gated spikes

- สร้าง Go module, package boundaries, CI, lint, unit/race/fuzz harness
- ทำ SIPGo interoperability spike ตาม gate
- spike Pion WebRTC G.711 passthrough, RTP mapping และ browser fake-audio
- spike embedded TURN UDP/TCP และ port allocation บน Windows Server 2022
- spike pure-Go SQLite WAL + Windows DPAPI capability key provisioning
- spike IIS WebSocket reverse proxy พร้อม Origin/header/timeout behavior
- บันทึก ADR และ re-estimate หลังผ่าน spike

Exit: dependency/port/media assumptions ผ่านบน target OS/PBX จริงทั้งหมด

### Phase 1 — Identity, persistence and service shell

- Windows Service lifecycle, installer paths, ACL และ recovery policy
- configuration validation, health/readiness, logging และ metrics
- SQLite schema/migrations/journal/outbox
- DPAPI capability key store และ overlap rotation
- local authenticated-encryption capability verifier โดยไม่มี Web API client

Exit: clean VM install/enroll/start/stop/restart ได้และ recovery tests ผ่าน

### Phase 2 — Portal session and WSS protocol

- API encrypted capability issue และ browser-relayed session lease/revoke
- IIS WSS endpoint, protocol parser, Origin allowlist/rate/size/heartbeat limits
- durable command deduplication, outbox ACK/replay และ snapshot
- session ownership, reconnect grace และ takeover rules
- Portal `runtimeMode` และ `SipGatewayAdapter`

Exit: authenticated browser session reconnect/replay/revoke ได้โดยไม่มี SIP/media

### Phase 3 — WebRTC and embedded TURN

- persistent PeerConnection, offer/answer/trickle ICE
- direct ICE UDP mux และ embedded TURN UDP/TCP
- browser audio device/gain/test/replaceTrack implementation
- media observability และ resource cleanup

Exit: browser-to-gateway bidirectional PCMU/PCMA media ผ่าน direct ICE และ TURN

### Phase 4 — SIP and call control

- Asterisk profile, REGISTER UDP/TCP และ refresh/retry/unregister
- outbound call capability verification และ full INVITE lifecycle
- inbound INVITE lifecycle และ unavailable policy
- serialized state machines, terminal-once behavior และ race handling
- RFC 4733 DTMF

Exit: SIP signaling acceptance matrix ผ่านโดยยังไม่ bridge production media

### Phase 5 — Media bridge and end-to-end integration

- RTP/RTCP mapping/bridge, symmetric RTP และ codec negotiation
- event relay compatibility และ business event verification
- failure/restart/outage/revoke/capability-key-rotation scenarios
- browser + IIS + Gateway + Issabel/Asterisk end-to-end tests

Exit: acceptance goals และ capacity/soak criteria ผ่านทั้งหมด

### Phase 6 — Packaging and pilot readiness

- signed release artifact, SBOM, configuration sample, install/rollback scripts
- firewall/DNS/IIS/ACME runbook
- dashboards, alerts, diagnostics และ incident runbook
- controlled Collection pilot ผ่าน explicit `runtimeMode=gateway`

Exit: release candidate พร้อม pilot approval; desktop agent ยังเป็น rollback path

## 14. Test plan

### 14.1 Unit, race and fuzz

- call/registration/session state transition tables
- duplicate command และ payload conflict
- terminal event exactly-once
- late answer after cancel, simultaneous hangup และ remote CANCEL races
- lease expiration, explicit disconnect และ Web API outage grace
- capability expiry/reuse/binding/key/origin/rate/message-size failures
- RTP sequence/timestamp wrap, packet reorder/loss และ codec mapping
- fuzz JSON envelope, SIP parser boundary, SDP และ stored recovery records
- `go test -race ./...` บน suite ที่รองรับ

### 14.2 Windows integration

- Windows Service install/start/stop/crash recovery
- DPAPI decrypt ได้เฉพาะ intended machine/service context
- DPAPI protected capability key file และ service-only ACL
- capability key provision/rotate/overlap/expiry
- SQLite WAL crash recovery, corrupt/incomplete journal และ retention bounds
- IIS WSS proxy, timeout, header forwarding และ Origin allowlist/wildcard policy
- firewall/port binding/restart บน Windows Server 2022 clean VM

### 14.3 Browser integration

- Playwright บน Edge และ Chrome ด้วย fake microphone/audio
- permission granted/denied/revoked
- input/output device selection, test sound, gain, mute และ `replaceTrack`
- WSS reconnect/snapshot/replay และ slow/duplicate/out-of-order messages
- direct ICE และ TURN fallback
- tab reload และ takeover behavior ตาม policy

### 14.4 SIP/PBX acceptance

- SIPp deterministic scenarios สำหรับ REGISTER/INVITE/CANCEL/ACK/BYE/retransmission/error
- Issabel/Asterisk จริงผ่าน UDP และ TCP
- inbound/outbound PCMU และ PCMA
- RFC 4733 DTMF
- busy, no-answer, auth failure, remote cancel/hangup และ malformed messages
- late 200 after CANCEL และ simultaneous BYE

### 14.5 Security and resilience

- one-time capability replay, stolen/mismatched capability และ expired capability
- raw destination injection และ unauthorized `callContextId`
- unknown/old capability key, ciphertext tamper, wrong issuer/audience/gateway และ rotation rollback
- logout/revoke/disabled user หยุดออก capability ใหม่และ terminate ภายใน lease bound
- Web API outage ทำให้ browser ต่อ lease/ขอ call capability ไม่ได้ และ Gateway terminate หลัง 120 วินาที
- log/metric/diagnostic secret scanning
- database/outbox thresholds 80%, 90%, hard limit และ terminal reserve
- malformed/flooded WSS, SIP และ TURN traffic ภายใน defined rate/quota

### 14.6 Capacity and soak acceptance

- 250 authenticated/registered sessions
- 200 concurrent established calls
- call setup/error rate อยู่ใน agreed test thresholds และไม่มี crash
- soak อย่างน้อย 2 ชั่วโมงและ 400 completed calls โดยคง workload เป้าหมายไว้
- ไม่มี unbounded memory, goroutine, socket, port, TURN allocation หรือ call-state leak
- หลัง test จบ registration/call/outbox/resource counters กลับสู่ expected baseline

## 15. Definition of done

P1 พร้อม pilot เมื่อ:

1. ทุก gated spike ผ่านและ ADR/dependency versions ถูกบันทึก
2. clean Windows Server 2022 ติดตั้ง executable/service และ provision DPAPI capability key ได้จาก runbook
3. Portal ใช้ `runtimeMode=gateway` โทรเข้า/ออกผ่าน Edge/Chrome ได้โดยไม่ติดตั้ง client agent
4. IIS WSS, direct ICE และ embedded TURN paths ผ่าน
5. Issabel/Asterisk UDP/TCP, PCMU/PCMA และ RFC 4733 matrix ผ่าน
6. security, revoke, outage, restart, replay และ capability key rotation tests ผ่าน
7. 250 sessions / 200 calls load smoke และ 2-hour soak ผ่านโดยไม่มี resource leak
8. logs/metrics/diagnostics ไม่มี prohibited secret หรือ media content
9. signed artifact, SBOM, configuration, install, operation และ rollback runbook พร้อม
10. desktop agent rollback ถูกทดสอบผ่าน explicit Collection runtime switch

## 16. Delivery estimate and planning assumptions

ประมาณการเริ่มต้น: วิศวกร 2 คน ใช้เวลา 8–12 สัปดาห์ รวม implementation, integration,
hardening, test automation และ pilot packaging โดยต้อง re-estimate หลัง Phase 0 เพราะความเสี่ยงหลัก
คือ SIPGo/Asterisk interoperability, Pion media passthrough, Windows capability-key lifecycle,
IIS WebSocket behavior และ network path สำหรับ ICE/TURN

ประมาณการนี้ถือว่า:

- มี Issabel/Asterisk environment และ VPN test clients ให้ใช้ตั้งแต่สัปดาห์แรก
- ทีม Web API/Portal สามารถเพิ่ม endpoint และ `runtimeMode` ตาม contract นี้
- มีสิทธิ์จัด DNS, ACME DNS-01, IIS, firewall และ Windows service account
- P1 ไม่ถูกขยายไปยัง transcoding, HA, recording หรือ public-internet deployment
- security/operations review เข้าร่วมตั้งแต่ Phase 0 ไม่รอท้ายโครงการ

หาก assumption ข้อใดเปลี่ยน ให้เปิด ADR/change request และ re-estimate ก่อนรวม scope เพิ่ม
