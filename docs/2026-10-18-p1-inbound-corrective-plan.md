# SIP Gateway P1 — Inbound Corrective Plan

วันที่: 18/10/2026

สถานะ: **Implemented and verified locally**

RequestFeedback: false — ผู้ใช้สั่งให้แก้ inbound ให้รองรับเต็มรูปแบบแล้ว

## เป้าหมาย

ทำให้ inbound call ผ่าน flow จริงครบตั้งแต่ PBX ส่ง INVITE, Portal แสดงสายเข้าและ ringtone,
รับ/ปฏิเสธสาย, ส่งเสียงสองทาง, remote CANCEL/BYE และ durable lifecycle ที่เข้ากันกับ Desktop Agent

## Root cause ที่ยืนยันแล้ว

Inbound INVITE ตั้ง remote RTP/RTCP endpoint สำเร็จตั้งแต่ `EventIncoming` แต่ `SIPGoRuntime.Answer`
ส่ง `EventConnected` โดยไม่มี remote SDP ตามธรรมชาติของ inbound dialog จากนั้น Coordinator ใช้ logic
ของ outbound ไป parse SDP ซ้ำ ทำให้จบสายเป็น `media_failed`

## งานแก้ไข

1. แยก connected-media handling ตาม call direction: outbound parse SDP answer; inbound ใช้ remote media
   ที่ validate และ bind ไว้ตอน INVITE
2. ทำ answer callback ให้เกิด terminal state ที่ถูกต้องเมื่อ media state หายหรือ transition ขัดแย้ง
3. เพิ่ม Gateway regression tests สำหรับ incoming → answering → connected → RTP สองทาง → BYE,
   reject, CANCEL และ invalid/unsupported SDP
4. เพิ่ม Web App regression สำหรับ inbound projection, answer/reject controls, ringtone stop และ connected audio
5. อัปเดต P1 result/acceptance notes และสร้าง Windows artifact ใหม่เมื่อ automated checks ผ่าน

## Verification

- `go test ./...`
- `go test -race ./internal/call ./internal/gateway ./internal/sip ./internal/media`
- `go vet ./...`
- Web App targeted inbound/phone-runtime tests, strict TypeScript และ production build
- `git diff --check`

ยังต้องใช้ SIPp + Issabel/Asterisk จริงทั้ง UDP/TCP และ PCMU/PCMA เป็น production release gate
เพราะ automated tests ไม่สามารถรับรอง PBX/network interoperability แทนสภาพแวดล้อมจริงได้
