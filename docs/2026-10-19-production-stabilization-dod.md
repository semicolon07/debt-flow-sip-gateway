# SIP Gateway production stabilization — Definition of Done

วันที่: 2026-10-19
สถานะรวม: AUTOMATED GATES COMPLETE; REAL ENVIRONMENT GATES PENDING

เอกสารนี้เป็น evidence ledger สถานะ `PASS` ต้องมี test/command หรือ artifact รองรับ ส่วน test ที่ต้องใช้
Windows Server, PBX, browser และ network จริงต้องคง `PENDING` จนกว่าจะรันจริง ห้ามอนุมานจาก unit test

## Automated correctness gates

| ID | Gate | Required evidence | Status |
|---|---|---|---|
| LIFE-01 | snapshot + lifecycle outbox commit/rollback เป็น transaction เดียว | store atomic commit และ forced rollback tests | PASS |
| LIFE-02 | async SIP/WebRTC state ไม่แซง durable state เมื่อ write fail | coordinator failure-injection regression | PASS |
| LIFE-03 | terminal state/event เกิดครั้งเดียวและเรียงตาม Phone Agent contract | outbound/inbound/refresh/restart lifecycle tests | PASS |
| EVT-01 | reconnect replay ถูกแบ่งหน้าและคง sequence | replay มากกว่า 1 page + cumulative ACK test | PASS |
| EVT-02 | 409/transient ingest ไม่ทำให้ ACK ข้าม event หรือ relay ค้างถาวร | Web App relay retry/recovery tests + API contract tests | PASS (automated) |
| CONC-01 | answer/reject/CANCEL/BYE/session replacement ไม่มี panic/data race | concurrency tests + `go test -race ./...` | PASS |
| CLEAN-01 | call/session/media resources ถูกคืนและใช้ซ้ำได้ทันที | cleanup/reuse tests; runtime counters กลับ baseline | PASS (automated); live soak pending |
| SIP-01 | initial INVITE, digest, provisional/final, ACK, CANCEL, BYE regression ผ่าน | SIP runtime test matrix UDP/TCP harness | PASS (automated) |
| SIP-02 | re-INVITE/UPDATE/PRACK/Require policy ไม่สร้าง call ซ้ำหรือค้าง dialog | in-dialog interoperability tests | PASS (automated) |
| SDP-01 | malformed SDP และ codec mismatch fail closed; valid target update ไม่ตัดสาย | SIP/coordinator SDP update tests | PASS |
| UI-01 | refresh/reconnect ไม่คืน terminal call/call bar และ device preferences คงอยู่ | Web App tests/build + browser test | PASS (automated); real browser pending |
| TEST-01 | Gateway unit/integration suite | `go test ./...` | PASS |
| TEST-02 | Gateway race detector | `go test -race ./...` | PASS |
| TEST-03 | Gateway static checks | `go vet ./...` | PASS |
| TEST-04 | Web App unit/lint/build | repository verification commands | PARTIAL — phone scope/build pass; 3 unrelated repository baselines remain |
| TEST-05 | Web API phone contract tests/build | targeted tests + solution build | PASS with existing dependency warnings |

## Performance gates

| ID | Gate | Required evidence | Status |
|---|---|---|---|
| PERF-01 | RTP steady-state hot path 0 B/op, 0 allocs/op | reusable-buffer benchmarks | PASS |
| PERF-02 | 200-call RTP benchmark ไม่ถดถอยจาก accepted baseline | `benchstat`/five-run comparison | PASS (local regression) |
| PERF-03 | control-plane/outbox benchmarks ไม่มี unexplained material regression | capability/protocol/snapshot/store results | PENDING — SQLite timing noisy; allocations stable |
| PERF-04 | no unbounded goroutine/heap/socket/port/TURN/outbox growth | Windows 2-hour soak evidence | PENDING |

## Real environment release gates

| ID | Gate | Required evidence | Status |
|---|---|---|---|
| REAL-01 | inbound/outbound และ error matrix ผ่านกับ Issabel/Asterisk จริงทั้ง UDP/TCP | SIPp/PBX/browser timelines | PENDING |
| REAL-02 | PCMU/PCMA bidirectional audio, early media, ringback และ terminal tone ถูกต้อง | sanitized metrics + operator result | PENDING |
| REAL-03 | 250 registered sessions / 200 concurrent calls | Windows load report | PENDING |
| REAL-04 | 2-hour soak / อย่างน้อย 400 completed calls และ resource กลับ baseline | sampler summary + reviewer sign-off | PENDING |
| REAL-05 | signed Windows package, preflight, rollback และ monitoring drill | release manifest/signature/ops evidence | PENDING |

รายละเอียด environment และ artifact layout ใช้ `docs/pilot-acceptance-matrix.md` เป็นหลัก

## Change log / evidence

- 2026-10-19: เพิ่ม atomic `SaveCallWithEvents` และทดสอบ commit/order/rollback
- 2026-10-19: เปลี่ยน reconnect replay จาก unbounded batch 10,000 เป็น page 256 events และทดสอบ drain ข้ามหน้า
- 2026-10-19: เพิ่ม replay barrier ให้ durable event ที่เกิดระหว่าง paged replay อยู่ใน SQLite จน browser
  ACK หน้าปัจจุบัน ป้องกัน sequence ใหม่แซง event เก่าที่ยังไม่ถูกส่ง และปฏิเสธ ACK ที่สูงกว่า
  delivered watermark โดยไม่แก้ outbox
- 2026-10-19: restart recovery บันทึก `call.state_changed(ended)` + `call.ended` เป็น atomic pair
  และ rollback ทั้งคู่เมื่อ capacity ไม่พอ; pre-authorization rejection ไม่ทิ้ง active call ที่ต้อง recover
- 2026-10-19: เพิ่ม startup validation ให้ outbox มีพื้นที่สำรองอย่างน้อย 2 terminal events ต่อ active call
- 2026-10-19: เพิ่ม SIP packet tests สำหรับ outbound/inbound dialog ทั้ง PRACK, UPDATE, re-INVITE,
  offerless re-INVITE, ACK และ unsupported `Require`
- 2026-10-19: `go test ./...`, `go test -race ./...` และ `go vet ./...` ผ่าน
- 2026-10-19: Web App phone-runtime targeted suite ผ่าน 99 tests; full suite ผ่าน 4,130 tests,
  production build/graph/performance budgets ผ่าน แต่ repository `verify` ยังรายงาน Node version mismatch,
  dynamic-form architecture baseline และ PrimeUI license policy ซึ่งไม่ได้เกิดจากงาน SIP รอบนี้
- 2026-10-19: Web API `PortalPhoneAgentStoreTests` ผ่าน 30 tests รวม Gateway outbound complete/busy
  lifecycle; solution build ผ่าน 0 errors พร้อม existing ImageSharp advisory warnings
- 2026-10-19: reusable RTP 200-call benchmark 34.66–37.42 ns/op, 0 B/op, 0 allocs/op;
  SQLite outbox allocation คงที่ 129 allocs/op แต่ timing แกว่งบน shared development host จึงไม่ปิด PERF-03
- 2026-10-19: rebuild unsigned Windows amd64 console artifact version
  `p1-stabilization-2026.10.19`; SHA-256 ของ executable/ZIP อยู่ใน `dist/SHA256SUMS`
  และ ZIP integrity test ผ่าน
- ทุก REAL gate, signed artifact และ load/soak ยังคง PENDING และเป็น release blocker
