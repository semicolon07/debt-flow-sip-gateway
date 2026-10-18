# SIP Gateway P1 — Inbound Corrective Plan

วันที่: 18/10/2026

สถานะ: **Implemented and verified locally**

RequestFeedback: false — ผู้ใช้สั่งให้แก้ inbound ให้รองรับเต็มรูปแบบแล้ว

## เป้าหมาย

ทำให้ inbound call ผ่าน flow จริงครบตั้งแต่ PBX ส่ง INVITE, Portal แสดงสายเข้าและ ringtone,
รับ/ปฏิเสธสาย, ส่งเสียงสองทาง, remote CANCEL/BYE และ durable lifecycle ที่เข้ากันกับ Desktop Agent

## Root cause ที่ยืนยันแล้ว

พบ defect อิสระกันห้าจุด:

1. Inbound INVITE ตั้ง remote RTP/RTCP endpoint สำเร็จตั้งแต่ `EventIncoming` แต่ connected callback
   ไม่มี remote SDP ตามธรรมชาติของ inbound dialog เดิม Coordinator กลับใช้ logic ของ outbound ไป parse
   SDP ว่างซ้ำ จึงจบสายเป็น `media_failed`
2. SIPGo ยุติ server transaction ทันทีเมื่อ `OnInvite` callback return แต่ implementation เดิมส่งเพียง
   `100/180` แล้ว return ก่อน browser กดรับสาย ทำให้ `call.answer` ภายหลังพยายามส่ง `200 OK` บน transaction
   ที่ปิดแล้วและได้ `sip_response_failed`

นอกจากนี้ matching `CANCEL` ถูก SIPGo consume ภายใน INVITE transaction จึงไม่วิ่งเข้า global
`OnCancel` handler ตามที่ implementation เดิมคาดไว้

3. registration durable sequence ถูก mark ว่า processed ฝั่ง browser แต่จะ ACK จริงเมื่อมี call event ตามมา
   เท่านั้น ทำให้ session ที่ยังไม่มีสาย replay/สะสม registration outbox
4. การสลับ microphone ไม่ disconnect Web Audio graph เดิม และ error ก่อนสร้าง processed track สำเร็จ
   ไม่ cleanup stream ทำให้ resource retention เพิ่มตามจำนวน retry/device switch
5. RFC 4733 packets ใช้ SSRC เดียวกับ audio แต่มี sequence generator คนละชุด ขณะ audio ยังไหลพร้อมกัน
   จึงมีโอกาสชน/ย้อน sequence ใน RTP stream เดียวกัน

## งานแก้ไข

1. แยก connected-media handling ตาม call direction: outbound parse SDP answer; inbound ใช้ remote media
   ที่ validate และ bind ไว้ตอน INVITE
2. คง INVITE callback ไว้จน final response/CANCEL/timeout/runtime close; ส่ง Answer/Reject แบบ async
   เพื่อไม่ block WSS reader ระหว่างรอ ACK และใช้ transaction-scoped cancel callback
3. ทำ answer callback ให้เกิด terminal state ที่ถูกต้องเมื่อ media state หายหรือ transition ขัดแย้ง
4. เพิ่ม Gateway regression tests สำหรับ UDP และ TCP `INVITE → 180 → 200 → ACK → BYE`, Reject 486,
   remote CANCEL, late CANCEL หลัง final 200, local hangup ระหว่างรอ ACK, mixed codec,
   invalid/unsupported SDP และตรวจ call map cleanup
5. เพิ่ม Web App regression สำหรับ inbound projection, answer/reject controls, ringtone stop และ connected audio
6. ACK registration sequence ที่ต่อเนื่องโดยไม่รอ call ingest พร้อม retry/gap guard และ backlog bound
7. cleanup Web Audio input graph ทุก success/failure/close path และแยก DTMF SSRC จาก audio stream
8. อัปเดต P1 result/acceptance notes และสร้าง Windows artifact ใหม่เมื่อ automated checks ผ่าน

## Zero-known-defect hardening รอบสุดท้าย

- `events.ack` เป็น correlated command: browser ขยับ cursor หลัง SQLite commit และ `command_result` เท่านั้น
- `session.welcome.acknowledgedThrough` เป็น cursor authoritative จาก Gateway ปิดช่อง ACK commit สำเร็จแต่
  response สูญหาย; cursor ข้าม sequence ของ user session อื่นได้โดยไม่ ACK แถวของผู้อื่น
- binding ของ durable event ถูกเก็บพร้อม session ตั้งแต่ redeem และ replay ข้าม page refresh ตาม
  `agentInstanceId + userSessionId` โดยคง Collection/binding/session ต้นทางสำหรับ Web API ingest
- event จาก Collection/session เก่าถูก relay เข้า API แบบ idempotent แต่ไม่เปลี่ยน call UI, ringtone หรือ media
  ของ runtime ปัจจุบัน
- WebSocket/PeerConnection รุ่นเก่าถูก fence ทุก callback และ async media setup เพื่อไม่ให้ close/answer/ICE/audio
  ที่มาช้าทำลาย socket หรือ peer รุ่นใหม่
- WebRTC reconnect ระหว่าง active call ต้องรักษา codec เดิม; retired peer ส่ง RTP/RTCP/ICE/state callback ไม่ได้
- RTP pair capacity นับเฉพาะ even RTP + odd RTCP ที่มีอยู่จริง และ P1 fail fast เมื่อ advertise IPv6
  เพราะ runtime socket เป็น IPv4

## Verification

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- Web App targeted inbound/phone-runtime tests, strict TypeScript และ production build
- `git diff --check`

ยังต้องใช้ SIPp + Issabel/Asterisk จริงทั้ง UDP/TCP และ PCMU/PCMA เป็น production release gate
เพราะ automated tests ไม่สามารถรับรอง PBX/network interoperability แทนสภาพแวดล้อมจริงได้
