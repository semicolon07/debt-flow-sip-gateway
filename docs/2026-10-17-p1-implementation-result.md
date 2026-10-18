# Debt Flow SIP Gateway P1 — Implementation Result

วันที่: 17/10/2026

สถานะ: **P1 engineering scope CLOSED 18/10/2026 / ยังไม่อนุมัติ production rollout**

การรับมอบครั้งนี้ปิดงาน implementation, contract integration และ automated verification ของ P1
ส่วน interoperability, clean-VM, signed release, load/soak และ operational evidence เป็น production release
gate ที่ยังต้องดำเนินการตาม `pilot-acceptance-matrix.md` การปิด P1 ไม่ได้เปลี่ยน gate ที่ยังเป็น `PENDING`
ให้เป็น `PASS` และไม่อนุญาตให้เปิด `sip.spikeApproved` จนกว่าจะมีหลักฐานครบ

## Architecture result

- Gateway ไม่มี URL, HTTP client หรือ credential สำหรับ Web API
- browser ขอ encrypted one-time session/call capability จาก API แล้ว relay ผ่าน WSS
- browser relay durable call event กลับ `call-events/ingest` ผ่าน pipeline เดียวกับ Desktop Agent
- Collection เลือก `desktop_agent` หรือ `gateway` แบบ explicit; ไม่มี automatic fallback

## Gateway result

- Windows Service executable, IIS WSS template, strict YAML และ DPAPI LocalMachine key provisioning
- startup config fail-fast เมื่อ ICE advertised IP ไม่ถูกต้อง หรือ TCP/UDP listener, RTP และ TURN relay
  port topology ชนกัน แทนการปล่อยให้ service/media allocation ล้มภายหลัง
- AES-256-GCM capability verification พร้อม exact issuer/audience/gateway/session/Collection/call binding,
  authenticated user-session binding, replay store และ cross-user session-resume protection
- SIPGo UDP/TCP client แยกตาม transport/local port, REGISTER digest, inbound/outbound dialog lifecycle,
  late-200 handling และ explicit RTP/RTCP endpoint parsing
- inbound Answer ใช้ remote RTP/RTCP endpoint ที่ validate จาก INVITE โดยไม่ parse SDP ว่างซ้ำจาก UAS
  connected callback; regression ครอบคลุม incoming → answering → connected, เสียงสองทาง, remote BYE,
  Reject 486 และ remote CANCEL
- Pion WebRTC PCMU/PCMA passthrough, RFC 4733, ICE UDP mux และ embedded TURN UDP/TCP
- TURN one-allocation-per-session/global quota, atomic slot reservation, bandwidth quota และ bounded metrics
- SQLite WAL command journal/outbox/ACK/replay/restart terminalization พร้อม size/retention guard
- SQLite schema v4 เก็บ persistent `agentInstanceId` ใน session outbox และ sequence cursor แยกตาม agent;
  session ใหม่ของ browser installation เดิมจึงไม่ reuse `(agentInstanceId, sequence)` ที่ Web API ใช้
  deduplicate, retention purge ไม่ทำให้ cursor reset และ restart terminal event ใช้ scope เดิมของ call
- WSS command error correlation, deterministic `welcome → replay → authoritative snapshot → live`, failed-takeover
  isolation และ registry ownership แยกตาม user session + Collection
- initial durable replay ใช้ bounded writer backpressure จึงส่ง replay ที่ยาวกว่า queue ได้ตามลำดับ;
  writer failure ถูกส่งกลับ admission loop และ live queue overflow ยัง fail closed
- WSS heartbeat ใช้ transport ping/pong เป็น liveness source; session ที่ไม่มี application command จะไม่ถูก
  idle timeout ผิดพลาด และ reader/writer failure ยกเลิก connection lifecycle ร่วมกัน
- WSS message admission ใช้ bounded sliding window จริง จึงไม่เปิดช่อง burst สองเท่าที่รอยต่อ fixed window
- registration state changes เข้า durable outbox ก่อน publish และ outbound/inbound call setup fail closed
  ก่อน SIP side effect เมื่อ call journal หรือ `call.created` outbox เขียนไม่สำเร็จ
- concurrent reconnect ถูก serialize เฉพาะ user-session + Collection เดียวกันตลอดช่วง runtime prepare
  ถึง registry commit ป้องกัน stale replacement cancel connection ที่ใหม่กว่าโดยไม่ block Collection อื่น
- `maxSessions` ถูกบังคับตั้งแต่ Coordinator capability redemption ก่อนสร้าง lease monitor/WebRTC
  จึงไม่มี unbounded pre-WebRTC session map แม้ client ที่ authenticate แล้วส่ง session IDs จำนวนมาก
- session resume ใช้ two-phase claim commit และ atomic PeerConnection replacement: connection เดิมยังใช้ได้
  จน replacement เปิดสำเร็จ และ failed replacement ไม่ทำลาย media/control เดิม
- session renewal ถูกผูกกับ connection capability ปัจจุบันและ re-check ก่อน commit; connection ที่ถูก
  replacement แล้วไม่สามารถต่อ lease ของ session ใหม่ได้
- SIP username มี owner ได้เพียง session เดียว; browser identity ใหม่ takeover session เก่าที่ idle แบบ atomic
  แต่ถูกปฏิเสธเมื่อ session เก่ายังมี active call เพื่อไม่ให้ REGISTER retry หรือ inbound routing แข่งกัน
- readiness ตรวจ SIP UDP/TCP listener จริงแบบ fail-fast, WebRTC ต้องอยู่สถานะ connected ก่อนเริ่ม/รับสาย
  และ PeerConnection failure จะจบ SIP dialog + media bridge แบบ terminal-once
- runtime hard limit และ package ตั้งไว้ที่ 250 sessions / 200 concurrent calls; shutdown SIP cleanup ทำแบบ
  bounded concurrent และ reconnect ไม่สร้าง lease-monitor ซ้ำโดยไม่จำเป็น
- TURN credential ต่ออายุตาม session lease, ปิด/zero secret แบบ race-safe และ symmetric RTP lock
  เฉพาะ source IP/port ที่เรียนรู้ครั้งแรก
- rotating JSON logs, Prometheus metrics/dashboard/alerts, allowlisted diagnostics + secret scan และ incident runbook
- outbox capacity alert คำนวณจาก limit ที่ตั้งจริง โดยใช้ pressure สูงสุดของ pending รวม, pending ต่อ session
  และ DB+WAL; warning ที่ 80% และ critical/block-call boundary ที่ 90% มี deployment contract test กำกับ
- SQLite maintenance/statistics failure มี bounded Prometheus counter และ critical alert แยกจาก checkpoint-busy gauge
- release script สร้าง Windows amd64 executable, CycloneDX SBOM, recursive checksums และรองรับ required
  Authenticode signing
- installer หยุด service ก่อน replace executable, register service SID ก่อนให้ ACL, จำกัด capability keys
  เป็น read-only แต่ให้เฉพาะ SQLite/log directories เขียนได้ และ fail เมื่อ SCM/start transition ไม่สำเร็จ;
  key provisioning รองรับทั้งก่อนและหลัง service registration
- release preflight ตรวจ Authenticode, executable hash ใน `SHA256SUMS`, CycloneDX 1.6 SBOM,
  build metadata, clean-source release manifest + binary version binding และปฏิเสธ config ที่เปิด
  plaintext capability keys ก่อนออก evidence
- SBOM ใช้เฉพาะ deduplicated module graph ที่ reachable จาก `cmd/sip-gateway` และ package build
  fail closed หาก `go list` สร้าง dependency inventory ไม่สำเร็จ จึงไม่รวม test/tool-only modules

## Debt Flow integration result

- Web API เพิ่ม `runtimeMode`, SIP `transport`, gateway session endpoint และ call capability โดยยังใช้
  call-context/event-ingest contract เดิม
- Web App เพิ่ม `SipGatewayAdapter`, WebRTC/audio controls, capability renewal/reconnect และ normalize event
  เข้า `PhoneEventRelayService` เดิม
- inbound `ringing` ถูก normalize เป็น `incoming` เพื่อเปิด Answer/Reject และ browser สร้าง local
  ringtone/ringback แบบ cadence ผ่าน output device/volume ที่ผู้ใช้เลือก; tone implementation ถูก lazy-load
  เพื่อให้ production bundle ยังผ่าน hard performance budget
- browser media แยก permission/device failure ที่ต้องให้ผู้ใช้ retry ออกจาก transient ICE/network reconnect
  เพื่อไม่ขอ microphone/capability วนเมื่อ permission ถูกปฏิเสธหรือ revoke
- browser audio ใช้ mono voice constraints (`echoCancellation`, `noiseSuppression`, `autoGainControl`),
  ติดตาม `devicechange` เพื่อ fallback ไป default microphone/speaker และมี inbound-RTP watchdog ที่
  retry playback/แจ้ง diagnostic โดยไม่ตัด SIP call อัตโนมัติ
- Gateway ขยาย shared ICE และ per-call RTP/RTCP UDP buffers, mark DSCP EF แบบ best-effort,
  และเพิ่ม packet-loss/jitter dashboard + alert โดย QoS ที่ OS ไม่รองรับจะ log warning และทำงานต่อ
- legacy data ที่ไม่มี runtime/transport normalize เป็น `desktop_agent` + `udp`

## Desktop Agent contract/lifecycle parity fix (2026-10-18)

ตรวจเทียบกับ `sip-windows-agent` ที่ `CallLifecyclePlanner` และ `CallReducer` แล้ว Gateway ส่ง lifecycle
เข้าสู่ `PhoneEventRelayService`/Web API ตามลำดับเดียวกัน:

- outbound: `created → dialing → trying → ringing → connected → ending → ended` แล้วตามด้วย `call.ended`
- inbound: `created → incoming → answering → connected → ending → ended` แล้วตามด้วย `call.ended`
- terminal transition ส่ง durable event เป็นคู่ `call.state_changed(ended)` + `call.ended` เหมือน Desktop Agent
- event sequence เพิ่มขึ้นทางเดียว, replay ใช้ business event ID เดิม และ cumulative ACK ยังห้ามข้ามช่องว่าง
- Web App Gateway adapter preserve `trying`, `incoming` และ `ending` แทนการแปลงเป็น `null`
- Web App publish snapshot `audioState=ready` ซ้ำเมื่อ WebRTC connected แม้ Gateway snapshot จะมาถึงก่อน
  ปิด race ที่โทรศัพท์ขึ้นพร้อมใช้งานแต่ปุ่มตั้งค่าเสียงยังถูก disable
- Gateway audio volume preferences ถูก validate และเก็บใน browser safe storage แยกจาก mute state
  ทำให้ระดับลำโพง/ไมโครโฟนคงเดิมหลัง refresh ขณะที่ mute ยังเริ่มแบบปลอดภัยเป็น unmuted
- Web API ยอมรับ legacy forward-only `created → ringing/connected` เพื่อ drain outbox จาก Gateway รุ่นก่อนหน้า
  โดยไม่ต้องลบ SQLite; Gateway รุ่นใหม่ไม่ใช้ทางลัดนี้แล้ว

Regression tests ครอบคลุม outbound lifecycle ทั้งเส้น, inbound `incoming/answering`, terminal event pair,
legacy API ingest และ Gateway-to-existing-durable-contract normalization

## Local verification

- `go test -race ./...` ผ่านทุก package รวม deployment-script ordering/least-privilege/alert-threshold contract tests
- architecture test parse production Go AST และ fail CI หากมีการสร้าง outbound `net/http` client หรือ
  import common HTTP client SDK เพื่อบังคับ invariant ว่า Gateway ไม่มี Web API control client
- .NET-issued encrypted session/call capability vectors ถูก verify ด้วย Go โดยตรง รวม AAD, JSON contract
  และ authenticated `userSessionId` binding
- capability security table tests ครอบคลุม wrong version/type/issuer/audience/gateway/Collection/protocol,
  future not-before, TTL/lease overflow, invalid SIP transport และ key rotation overlap/removal
- fuzz harness ครอบคลุม WSS JSON envelope, SIP parser boundary, SDP และ stored recovery records;
  short fuzz run ผ่านโดยไม่พบ crash
- SIPp harness มี contract test ตรวจ XML/runner registration และ scenario สำหรับ digest/auth failure,
  PCMU/PCMA, busy, no-answer/CANCEL, inbound CANCEL, unsupported codec, malformed SDP และ late-200
- `go vet ./...` และ Windows amd64 CGO-disabled cross-build ผ่าน
- PowerShell 7.5.4 parser ตรวจ deployment scripts ทุกไฟล์ผ่าน; unsigned packaging smoke สร้าง PE32+ amd64,
  release manifest, CycloneDX 1.6 SBOM และ recursive `SHA256SUMS` ที่ verify ได้ครบ; Windows CI มี
  parser gate ซ้ำก่อน build
- `govulncheck` ไม่พบ vulnerability ใน symbol/package ที่ใช้งาน; อัปเดต `x/net` เป็น `v0.60.0`
  เพื่อล้าง HTTP/2 module advisories และเพิ่ม reachable-vulnerability gate ใน CI
- Web App `npm audit --omit=dev` ไม่พบ production dependency vulnerability
- Web API dependency scan พบ baseline เดิมที่ยังต้องปิดก่อน pilot: `SixLabors.ImageSharp 3.1.12`
  มี 3 high + 2 moderate advisories ใน production graph (เวอร์ชันที่แก้ครบคือ `4.1.2` ซึ่งเป็น major
  upgrade); ทดลองทั้ง patched minimum `4.1.2` และ latest `4.1.3` แล้ว restore ผ่าน แต่ทั้งสอง package
  intentionally fail build เมื่อไม่มี `SixLaborsLicenseKey`/license file จึงคืน baseline เพื่อไม่ทิ้ง build แตก
  ต้องให้เจ้าของระบบจัดหา/อนุมัติ license key หรืออนุมัติ library replacement ก่อนดำเนินการ ส่วน
  `SQLitePCLRaw.lib.e_sqlite3 2.1.11`
  ที่พบใน API test graph ถูก pin เป็น `2.1.13` แล้ว พร้อมรัน API 870 tests และ rescan ยืนยันว่า advisory หาย
- benchmark harness ครอบคลุม capability/JSON/RTP 200 streams/runtime snapshot 250 sessions+200 calls/SQLite;
  live Windows sampler เก็บ normalized CPU, working/private memory, Go heap, sockets และ Gateway high-water marks
- Web API Infrastructure 2,573 tests ผ่าน (90 tests skip ตาม environment) รวม legacy Gateway ingest regression
- Web API `dotnet format --verify-no-changes --no-restore` ผ่าน
- Web App: 570 test files / 4,057 tests, strict app/spec และ production build ผ่าน;
  microphone/speaker test paths ปิด media tracks และ `AudioContext` ผ่าน `finally` แม้ browser API ล้มกลางทาง
- diagnostics allowlist report ผ่าน automated secret-leak test และ manual pattern scan
- local browser POC ที่ `test/poc/sip-gateway-poc.html` ครอบคลุม API-assisted/manual capability,
  WSS/WebRTC, direct/TURN, registration, inbound/outbound, DTMF, device switching และ sanitized log;
  dev HTTP origin ใช้ได้เฉพาะ loopback เมื่อ opt-in และ production preflight ปฏิเสธ flag นี้

Frontend `npm run verify` ยัง exit 1 จาก baseline นอก scope สามรายการ: Node ในเครื่องเป็น 26.0.0
แทน pinned 22.22.3, dynamic-form date-control debt และ PrimeUI license disclosure policy; test/build ของ feature ผ่าน

## Definition-of-done audit

| DoD | Local evidence | Remaining proof |
|---|---|---|
| dependency/ADR/gated spikes | pinned Go modules, ADR, race/fuzz/SIPp contract tests | target Windows/PBX/browser spike |
| Windows service + DPAPI | cross-build, installer/provision/preflight scripts และ deployment contract tests | clean Windows Server 2022 run |
| browser inbound/outbound | Gateway adapter, shared event relay และ 4,037 Web App tests | Edge/Chrome + real audio end-to-end |
| IIS WSS/direct ICE/TURN | exact-Origin WSS, ICE/TURN implementation/config | IIS/private-network route evidence |
| SIP/media/DTMF | SIPGo/Pion/RTP tests และ 13 SIPp scenarios | Issabel/Asterisk UDP/TCP matrix |
| security/recovery | capability/replay/restart/outbox/diagnostic tests; Gateway/Web App scans ผ่าน | Web API baseline dependency remediation/approval และ revoke/outage/key-rotation drill on target |
| capacity/soak | benchmark/load sampler พร้อม bounded quotas | 250 sessions/200 calls/2-hour report |
| secret-free operations | allowlisted diagnostics + automated secret scan | reviewer-approved pilot bundle |
| signed release package | Authenticode-ready build, checksum/SBOM/preflight verification | production certificate + approvals |
| explicit rollback | `desktop_agent` default, explicit Collection runtime switch | operator rollback drill |

## External gates before pilot

1. SIPp + Issabel/Asterisk จริงครบ UDP/TCP, PCMU/PCMA, DTMF, retransmission, CANCEL/late-200/BYE
2. Windows Server 2022 clean VM: service/IIS/ARR/firewall/DPAPI/ACL/restart และ PowerShell packaging
3. Edge/Chrome อุปกรณ์เสียงจริง, direct ICE/TURN fallback และ browser/API/Gateway event ingest end-to-end
4. 250 registered sessions, 200 calls, 2-hour soak และ resource return-to-baseline
5. Web API baseline dependency remediation: อนุมัติ/จัดหา Six Labors license key สำหรับ ImageSharp
   `4.1.3` หรืออนุมัติ library replacement แล้วรัน regression/security scan ใหม่
6. production Authenticode certificate, SBOM/license/security review และ operations approval

Repository มี Windows preflight, SIPp deterministic scenarios และ pilot evidence matrix แล้ว แต่รายการ
external gate ยังเป็น `PENDING` จนกว่าจะรันใน target environment และมี reviewer อนุมัติจริง ดูวิธีวัดและ
local regression baseline ใน [performance and capacity testing](performance-testing.md)

ห้ามตั้ง `sip.spikeApproved: true` หรือเปิด Collection pilot จน external gates ข้างต้นผ่าน
