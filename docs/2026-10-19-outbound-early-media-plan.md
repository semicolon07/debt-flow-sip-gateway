# Outbound Early Media และ Terminal Tone Plan

วันที่: 2026-10-19
สถานะ: อนุมัติให้ดำเนินการแล้ว

## เป้าหมาย

ทำให้ outbound SIP behavior ใกล้เคียง softphone จริง โดยรองรับ `183 Session Progress + SDP` และส่ง early RTP จาก PBX/carrier ไปยัง WebRTC ก่อน `200 OK` พร้อมแยกออกจาก durable call lifecycle ที่ Web API ใช้บันทึกข้อมูล

## ขอบเขตและ scenario matrix

- `100 Trying` คงสถานะ trying โดยไม่เริ่ม media
- provisional response ที่ไม่มี SDP (`180`, `183` หรือรหัส `1xx` อื่น) ใช้ synthetic ringback
- provisional response ที่มี SDP (`180`, `183` หรือรหัส `1xx` อื่น) เปิด early media
- ตั้ง remote RTP/RTCP ทันทีเมื่อ SDP และ codec ถูกต้อง
- ส่ง live-only `call.early_media` ไปยัง browser เพื่อหยุด synthetic ringback หลังตรวจพบสัญญาณเสียงจริงใน G.711 ต่อเนื่องหลาย packet ไม่ใช่เพียงเมื่อได้รับ SDP, RTP silence หรือ transient noise
- เก็บสถานะ early media ใน gateway snapshot เพื่อรองรับ reconnect
- SDP ชั่วคราวที่ไม่ถูกต้องหรือ codec ไม่ตรงต้องไม่ทำให้สายล้ม เพราะ provisional หรือ final response ถัดไปอาจถูกต้อง
- provisional SDP ซ้ำเปลี่ยน RTP target ได้ แต่ไม่ส่ง activation event ซ้ำ
- provisional SDP ที่ยังไม่มี RTP หรือได้รับเฉพาะ PCMU/PCMA digital silence ต้องคง synthetic ringback เพื่อไม่ให้ผู้ใช้ได้ยินความเงียบ
- final `2xx` ที่มี SDP ใช้ SDP สุดท้าย; final `2xx` ที่ไม่มี SDP ใช้ early-media target เดิมได้
- ไม่ส่ง browser microphone RTP ไป PBX จนกว่าสายจะ `connected`
- final `3xx`–`6xx`, timeout, local cancel, late answer, duplicate final และ remote hangup ต้องยัง terminalize/cleanup ได้ครั้งเดียว
- เพิ่ม regression tests ที่ระดับ SIP runtime, coordinator และ Web App

## เกณฑ์ยอมรับ

- `180` ที่ไม่มี SDP ยังใช้ synthetic ringback ตามเดิม
- `180/183/1xx + SDP` เตรียมเส้นทาง media และหยุด synthetic ringback เมื่อ G.711 RTP มีระดับสัญญาณเสียงจริงต่อเนื่อง 5 packet ไปถึง WebRTC
- provisional response ที่ไม่มี SDP fallback เป็น ringing
- final response ยัง terminalize และ cleanup ได้ครั้งเดียว
- early-media event ไม่เข้า durable outbox และไม่ถูกส่งไป ingest API
- stale/unknown early-media event ไม่สามารถหยุด tone ของสายปัจจุบันได้

## ขอบเขต protocol ที่ประกาศรองรับ

Gateway ไม่ advertise SIP extension `100rel` จึงไม่ร้องขอ reliable provisional response/PRACK;
PBX ปกติต้องใช้ ordinary provisional response กับ client ที่ไม่ได้ประกาศ extension นี้ หรือปฏิเสธ
INVITE ด้วย `421 Extension Required` ซึ่งจะจบสายอย่างปลอดภัยในกลุ่ม `sip_failed` แทนการค้างสาย
