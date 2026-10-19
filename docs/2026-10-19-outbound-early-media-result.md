# ผลการรองรับ Outbound Early Media

วันที่: 2026-10-19

## สรุป

Gateway ส่งเสียงประกาศหรือ tone จาก PBX/carrier ที่มากับ provisional SDP ไปยัง WebRTC ได้ก่อน
`200 OK` แล้ว โดย lifecycle สำหรับ Web API ยังคงใช้ contract เดิมและ `call.early_media` เป็น live-only
ไม่ถูกเขียนลง durable outbox

## พฤติกรรมที่รองรับ

- `100 Trying` ไม่เปิด media
- `180/183/1xx` ที่ไม่มี SDP ใช้ browser synthetic ringback
- `180` ที่มี SDP และ codec PCMU/PCMA ตรงกับสาย เตรียม/forward early RTP/RTCP แต่คง synthetic ringback ตาม SIP ringing semantics
- `183/1xx progress` ที่มี SDP เตรียม early RTP/RTCP และคง synthetic ringback จนตรวจพบสัญญาณเสียงจริง
- Gateway ส่ง `call.early_media` หลังส่ง G.711 RTP ที่ผ่าน activity detection ต่อเนื่อง 5 packet เข้า WebRTC สำเร็จเท่านั้น; SDP อย่างเดียว, payload ว่าง, DTMF RTP, digital silence หรือ transient noise ไม่ทำให้ ringback เงียบ
- provisional SDP ผิดรูปแบบ, ไม่มี audio target หรือ codec ไม่ตรง ถูก ignore โดยสายไม่ล้ม และ recovery ได้เมื่อ SDP ถัดไปถูกต้อง
- provisional SDP ซ้ำอัปเดต RTP target ได้โดยไม่ส่ง `call.early_media` ซ้ำ
- `200 OK` ที่มี SDP ใช้ final target; ถ้าไม่มี SDP จะ reuse target ที่ negotiate จาก early media
- browser รับเสียง early media ได้ แต่ microphone RTP ถูก block จน call state เป็น `connected`
- reconnect ระหว่าง early media คืน `gateway.snapshot.earlyMedia=true` เฉพาะเมื่อเคยตรวจพบสัญญาณเสียงจริง
- `call.early_media` ไม่ถูก ACK/ingest และไม่เปลี่ยน contract ของ Web API
- stale event ที่ไม่ตรงกับ durable `call.created` ถูก Web App ปฏิเสธ
- final failure, timeout, cancel, late answer, duplicate terminal event และ remote hangup ยังคง cleanup ตาม authoritative lifecycle เดิม

## การตรวจสอบ

- `go test ./...`: ผ่านทุก package
- `go test -race ./...`: ผ่านทุก package โดยไม่พบ data race
- Gateway early-media coordinator tests: ผ่าน รวม `180 + audible RTP` ที่ต้องคง local ringback, การเปลี่ยน `180 → 183`, SDP ที่ยังไม่มี RTP, DTMF, PCMU digital silence, sustained audible G.711, RTP receive-only ก่อน answer, duplicate SDP, malformed SDP recovery และ final response ไม่มี SDP
- G.711 activity detector tests: ผ่านสำหรับ PCMU/PCMA silence, audible signal, truncated payload และ unsupported codec
- SIP runtime tests: ผ่านสำหรับ `183 + SDP`, `180 + SDP` และ `183` ไม่มี SDP
- Web App tests เฉพาะ adapter/tone: 53 tests ผ่าน
- Web App full suite: 572 test files / 4,116 tests ผ่าน
- Web App strict app/spec checks และ production build: ผ่าน
- Windows amd64 executable และ POC package ถูก build ใหม่ พร้อมตรวจ SHA-256 ทั้ง package ภายในและไฟล์ส่งมอบ

## ขอบเขต

Gateway ไม่ advertise `100rel` และไม่ได้ร้องขอ PRACK การเชื่อมต่อกับ PBX ที่บังคับ extension นี้ต้อง
ปรับ PBX ให้ยอมรับ ordinary provisional response หรือเพิ่ม PRACK เป็น compatibility feature แยกต่างหาก
เพื่อไม่เพิ่มความเสี่ยงให้เส้นทาง SIP ที่ใช้งานอยู่โดยไม่มี negotiation
