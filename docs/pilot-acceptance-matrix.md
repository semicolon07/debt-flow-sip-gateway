# SIP Gateway P1 pilot acceptance matrix

เอกสารนี้เป็น evidence checklist ไม่ใช่คำรับรองว่า gate ผ่าน ผู้ทดสอบต้องกรอก environment, commit,
artifact path และผู้อนุมัติ ห้ามเปลี่ยน `sip.spikeApproved` เป็น `true` หากมีแถว `PENDING/FAIL`

P1 engineering scope ปิดรับมอบเมื่อ 18/10/2026 แล้ว แต่ checklist นี้ยังเป็น production release track
ที่ต้องผ่านก่อน rollout จริง และไม่ถือว่าเป็น feature scope ของ P2

ผู้ใช้ยืนยัน functional flow ของ corrective build บนระบบที่ deploy แล้วเมื่อ 18/10/2026 รวมการกลับมาใช้งาน
หลัง refresh โดยไม่มี terminal Call Bar ค้าง การยืนยันนี้ปิด P1 functional acceptance แต่ไม่เปลี่ยนสถานะ
ของ non-waivable gates ด้าน transport/browser matrix, capacity, soak, security และ signed release ด้านล่าง

## Run identity

| Field | Value |
|---|---|
| Gateway commit/version | PENDING |
| Web API commit/version | PENDING |
| Web App commit/version | PENDING |
| Windows Server build/VM image | PENDING |
| Issabel/Asterisk version | PENDING |
| Edge/Chrome versions | PENDING |
| Network/VPN profile | PENDING |
| Started/finished UTC | PENDING |
| Operator/reviewer | PENDING |

## Non-waivable gates

| ID | Gate | Required evidence | Status |
|---|---|---|---|
| WIN-01 | Clean Windows Server 2022 install/service/ACL/DPAPI/restart | signed package hash, `sip-gateway-preflight.json`, service event log, restart terminal event | PENDING |
| IIS-01 | IIS TLS/WSS Origin policy, timeout and header forwarding | TLS certificate details, accepted configured origins, rejected wrong Origin (หรือ explicit wildcard risk acceptance), reconnect trace | PENDING |
| NET-01 | Gateway has no route to Web API | preflight with `-RequireReleaseEvidence -ForbiddenApiHost ...`, firewall rule export | PENDING |
| SIP-01 | REGISTER digest refresh/unregister UDP+TCP | SIPp result/message/stat files and Asterisk log | PENDING |
| SIP-02 | Outbound INVITE/ringing/answer/CANCEL/ACK/BYE/errors UDP+TCP | SIPp evidence plus API timeline | PENDING |
| SIP-03 | Inbound answer/reject/CANCEL/BYE UDP+TCP | SIPp evidence plus API timeline | PENDING |
| SIP-04 | late-200, simultaneous hangup, retransmission, malformed SIP/SDP | SIPp traces proving terminal-once | PENDING |
| MED-01 | PCMU and PCMA bidirectional RTP/RTCP | packet counters, loss/jitter, codec evidence; no media capture in support bundle | PENDING |
| DTMF-01 | RFC 4733 digits reach PBX without log leakage | PBX DTMF result and secret-scan result | PENDING |
| WEB-01 | Edge and Chrome fake audio/permission/device/gain/mute/replaceTrack | Playwright trace/report for both engines | PENDING |
| ICE-01 | direct ICE and forced TURN UDP/TCP | selected-route metrics and TURN allocation/bytes/cleanup | PENDING |
| EVT-01 | browser relays Gateway events through existing API ingest exactly once | event IDs/sequences, API accepted/duplicate response, Gateway ACK watermark | PENDING |
| SEC-01 | capability replay/mismatch/expiry/key rotation/revoke/outage | test report and sanitized metrics; no token/key values | PENDING |
| DEP-01 | Production/test dependency advisories remediated | clean Gateway/Web App scans; Web API ImageSharp 4.1.3 license key or approved replacement, clean rescan and approval | PENDING |
| LOAD-01 | 250 registered sessions / 200 established calls | load report with CPU, working set, GC, sockets, ports, call setup p95 and error rate | PENDING |
| SOAK-01 | 2 hours / at least 400 completed calls while sustaining the target workload | time-series export and return-to-baseline comparison | PENDING |
| OPS-01 | alerts/dashboard/diagnostics/rollback drill | alert screenshots/export, sanitized bundle, explicit runtime switch timeline | PENDING |
| REL-01 | Authenticode/SBOM/license/security approval | signature verify, clean-source release manifest/binary version binding, checksums, SBOM review and approvals | PENDING |

Automated stabilization วันที่ 19/10/2026 เพิ่ม regression coverage สำหรับ lifecycle atomicity, replay,
PRACK, UPDATE, re-INVITE/offerless re-INVITE, Require policy และ Gateway event contract แล้ว แต่หลักฐานนี้
ไม่เปลี่ยนสถานะ SIP/WEB/EVT/LOAD/SOAK ด้านบน: แต่ละ gate ยังต้องเก็บ trace จาก Windows Server,
Issabel/Asterisk และ browser จริงตาม Required evidence ก่อน rollout

## Commands and evidence layout

Build the release with required signature, then run on the clean VM:

```powershell
.\scripts\build.ps1 -Version <version> -OutputDirectory .\dist\release\<version> `
  -CodeSigningCertificateThumbprint <thumbprint> -RequireSignature
.\scripts\test-pilot-preflight.ps1 -RequireReleaseEvidence `
  -ForbiddenApiHost api-debt-flow.uniga.app -ExpectedSignerThumbprint <thumbprint>
```

Run each SIPp scenario for both transports. The PBX address inside the issued SIP bootstrap must point to
the SIPp host/port for this isolated test:

```powershell
.\scripts\run-sipp-matrix.ps1 -Scenario register-digest -Transport udp `
  -GatewayHost 10.0.0.10 -LocalIp 10.0.0.20 -Username 1001
```

Store evidence under an immutable run folder using this minimum layout:

```text
pilot-evidence/<run-id>/
  manifests/commits-and-versions.json
  windows/sip-gateway-preflight.json
  sipp/<scenario>-<transport>/*
  browser/{edge,chrome}/
  api/event-timelines/
  metrics/{before,during,after}/
  security/secret-scan.txt
  approvals/
```

Evidence must not contain capability tokens, SIP credentials, Authorization headers, SDP, ICE/TURN
credentials, raw phone numbers, DTMF sequences or media. Packet capture requires a separate
security-approved location and must not be included in diagnostics/support bundles.

ใช้ `scripts\run-local-benchmarks.ps1` สำหรับ regression baseline และ
`scripts\observe-live-capacity.ps1` ระหว่าง 250-session/200-call workload ตาม
`docs\performance-testing.md`; benchmark local ห้ามใช้แทน `LOAD-01/SOAK-01`
