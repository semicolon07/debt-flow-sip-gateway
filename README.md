# Debt Flow SIP Gateway

SIP Gateway สำหรับให้ Debt Flow Portal โทรผ่าน browser ด้วย WebRTC โดยไม่ต้องติดตั้ง
Debt Flow SIP Windows Agent ที่เครื่องผู้ใช้

สถานะปัจจุบัน: **P1 functional scope accepted and closed (18/10/2026); production release qualification pending**

Multi-profile target 26/10/2026: Gatewayกลางหนึ่งตัวรองรับหลาย PBX profilesได้โดย capabilityเดิมส่ง
host/port/transport/credentialต่อ session. Sourceวันที่ 27/10/2026เปลี่ยน in-process SIP owner keyจาก
Usernameอย่างเดียวเป็น canonical `host + port + transport + realm + username`แล้ว พร้อม opaque Contactและ
PBX-peer fallbackสำหรับ inbound routing. Target contractอยู่ที่
[FEAT-049 multi-profile architecture](https://git.uniga.app/debt-flow/docs/-/blob/main/features/collection-sip-provider/multi-profile-architecture.md);
ยังต้องผ่าน Issabel/Asterisk multi-PBX pilotก่อน production. Web API targetใช้ endpoint registryที่มี shared capability
key ringและ GatewayIdแยกต่อ endpoint; Gatewayทุกตัวต้อง verify keyร่วมและ reject `gatewayId`ที่ไม่ตรง. ไม่มี endpoint
fallbackหรือ Web API→Gateway management/revocation channel. Administrative Disable SLA 10 วินาทีอาศัย Portalที่ยัง
onlineส่ง hangup/close; disconnected sessionยังใช้ Gateway lease/reconnect graceเดิม.

เอกสารหลัก:

- [P1 implementation plan](docs/2026-10-17-p1-implementation-plan.md)
- [P1 implementation result](docs/2026-10-17-p1-implementation-result.md)
- [P2 candidate roadmap](docs/2026-10-18-p2-roadmap.md)
- [Windows deployment](docs/windows-deployment.md)
- [Incident runbook](docs/incident-runbook.md)
- [Pilot acceptance matrix](docs/pilot-acceptance-matrix.md)
- [Performance and capacity testing](docs/performance-testing.md)
- [Local monitoring and embedded diagnostics](docs/local-monitoring.md)
- [Monitoring dashboard implementation result](docs/2026-10-18-monitoring-dashboard-result.md)
- [Session media continuity corrective result](docs/2026-10-19-session-media-continuity-result.md)
- [Production stabilization plan](docs/2026-10-19-production-stabilization-plan.md)
- [Production stabilization Definition of Done](docs/2026-10-19-production-stabilization-dod.md)
- [Local browser POC](test/poc/README.md)

P1 baseline ที่ยืนยันแล้ว:

- Go และ Windows Server 2022 แบบ single-node Windows Service
- IIS รับ `wss://sip-gateway.uniga.app/gateway/v1` และ reverse proxy ไปยัง gateway
- Pion WebRTC v4 และ Pion TURN v5 แบบฝังใน executable เดียว
- Issabel/Asterisk ผ่าน SIP UDP/TCP รองรับทั้ง inbound และ outbound
- PCMU/PCMA passthrough และ RFC 4733 DTMF โดยไม่มี transcoding
- SQLite WAL และ Windows DPAPI สำหรับ state/key material ที่ต้องคงอยู่
- เป้าหมาย 250 registered sessions และ 200 concurrent calls บน private/VPN network

P1 ยังเก็บ `sip-windows-agent` เดิมไว้เป็น rollback path ผ่านการตั้งค่า `runtimeMode`
ระดับ Collection และไม่มี automatic fallback ระหว่าง runtime

ผู้ใช้ยืนยัน corrective build บนระบบที่ deploy แล้วเมื่อ 18/10/2026 รวม recovery หลังจบสายและ refresh
โดยไม่มี terminal Call Bar ค้าง การยืนยันนี้ปิด P1 functional acceptance แต่ไม่แทน load/soak,
signed-release, security และ full interoperability evidence ใน pilot matrix

## การเชื่อมต่อ

```text
Browser ──HTTPS──> Web API       (authorize และออก encrypted capability)
Browser ───WSS───> SIP Gateway   (relay capability + call control)
Browser <─WebRTC─> SIP Gateway <─SIP/RTP─> Issabel/Asterisk
```

ไม่มี connection จาก SIP Gateway ไป Web API ทั้งใน configuration และ implementation; browser เป็นผู้
relay capability และ call event เหมือน boundary ของ Desktop Agent เดิม

## Resource profile

ภาระหลักขึ้นกับ concurrent calls: ICE/DTLS/SRTP และ TURN relay ใช้ CPU มากที่สุด ส่วน
PeerConnection/RTP buffers และ unacknowledged outbox ใช้ memory หลัก P1 ใช้ PCMU/PCMA passthrough
โดยไม่ transcode เพื่อลด CPU ค่า production target คือ 250 sessions / 200 concurrent calls;
ตัวเลขนี้เป็น hard-limit และ load-test target ไม่ใช่ sizing guarantee จนกว่าจะผ่าน soak บน Windows Server จริง;
ต้อง benchmark บน Windows Server จริงก่อนกำหนด production capacity

## สิ่งที่ implement แล้ว

- Windows Service executable, strict YAML configuration, IIS reverse-proxy template, Authenticode-ready
  PowerShell packaging, CycloneDX SBOM และ sanitized diagnostics bundle
- exact/multi-Origin WSS allowlist (พร้อม explicit `*`), protocol V1, one-time authentication,
  lease/reconnect, takeover protection และ bounded queues
- SQLite WAL command journal, per-agent monotonic event sequence, durable session outbox, ACK/replay และ
  deterministic restart terminalization
- bounded rotating JSON operational logs พร้อม production `info`/sanitized diagnostic `debug`,
  Prometheus dashboard/alerts และ allowlisted diagnostics
- read-only embedded diagnostics ที่ `127.0.0.1:9090/diagnostics/` พร้อม CPU, working set, Go memory,
  sessions/calls, SIP registration, ICE/TURN, outbox และ SQLite snapshot
- Pion WebRTC/ICE, embedded TURN UDP/TCP, G.711 RTP mapping, RFC 4733 DTMF และ browser audio controls
- SIPGo REGISTER/INVITE lifecycle หลัง `sip.spikeApproved` gate พร้อม inbound/outbound call state
- local AES-256-GCM session/call capability verification โดย Gateway ไม่เชื่อม Web API
- browser-relayed capability contracts และ Collection `runtimeMode`
- Portal `SipGatewayAdapter` สำหรับ WSS/WebRTC โดย legacy `desktop_agent` ยังเป็นค่าเริ่มต้น

## Production release gate ที่ยังต้องผ่าน

- SIPp และ Issabel/Asterisk จริงทั้ง UDP/TCP, PCMU/PCMA, late-200/CANCEL และ retransmission matrix
- Windows Server 2022 + IIS + CNG/DPAPI clean-VM validation
- DPAPI capability-key provisioning/rotation บน Windows และ secret-manager rotation ฝั่ง Web API
- RTCP interoperability, Edge/Chrome device matrix, 250-session/200-call load test และ 2-hour soak
- production certificate signing, SBOM/license/security review และ operational approval

Gateway จะตอบ readiness เป็น `503` เมื่อ `sip.spikeApproved: false` เพื่อป้องกันการเปิดใช้ก่อนผ่าน
interoperability gate และไม่มีการสลับกลับไป Desktop Agent อัตโนมัติ
