# SIP Gateway production stabilization plan

วันที่เริ่ม: 2026-10-19
สถานะ: AUTOMATED STABILIZATION COMPLETE — REAL QUALIFICATION PENDING
ขอบเขต: defect correction, SIP interoperability, reliability และ performance เท่านั้น

## เป้าหมาย

ทำให้ระบบปัจจุบันมีพฤติกรรมคงที่และตรวจสอบย้อนหลังได้สำหรับ 250 browser sessions / 200 concurrent calls
โดยหยุดเพิ่ม product feature จนกว่า Definition of Done ใน
`docs/2026-10-19-production-stabilization-dod.md` จะผ่านครบตามประเภทหลักฐาน

คำว่า zero defect ในแผนนี้หมายถึงไม่มี known reproducible defect ค้างอยู่ใน flow ที่ประกาศรองรับหลังผ่าน
automated regression และ pilot matrix ไม่ใช่คำรับรองว่า software ไม่มี defect ที่ยังไม่ถูกค้นพบ ส่วน zero allocation
กำหนดเฉพาะ RTP packet translation steady-state hot path; control plane, JSON, SQLite, SIP และ WebRTC library
ยังมี allocation ตามธรรมชาติและวัดเป็น regression budget แทน

## หลักการและข้อห้ามระหว่าง stabilization

- หยุดเพิ่ม feature, codec, transport หรือ deployment topology ใหม่
- Gateway ต้องไม่เรียก Web API โดยตรง; browser เป็นผู้ relay durable event ตามสถาปัตยกรรมเดิม
- ห้าม ACK หรือทิ้ง durable event ที่ Web API ยังไม่ยอมรับเพื่อกลบ `409`
- call snapshot กับ lifecycle event ที่สังเกตจากภายนอกต้อง commit แบบ atomic
- asynchronous SIP/WebRTC transition ต้องไม่ขยับ in-memory state แซง durable state
- protocol ที่รองรับไม่ครบต้องตอบ SIP อย่างชัดเจนและไม่ทำให้ dialog/call slot ค้าง
- ทุก bug fix ต้องมี regression test ที่ล้มกับ behavior เดิมได้ หรือมีเหตุผลบันทึกว่าทำไมต้องทดสอบบนระบบจริง
- ห้ามใช้ local benchmark แทนผล Windows/PBX/browser load และ soak

## Workstream และลำดับดำเนินการ

### S1 — Durable lifecycle และ recovery

1. รวม call snapshot และ event outbox ใน SQLite transaction เดียว
2. ใช้ copy/apply/commit/swap กับ asynchronous call state เพื่อ rollback in-memory state เมื่อ persist ล้มเหลว
3. terminal transition ต้องบันทึก `call.state_changed(ended)` และ `call.ended` เป็นกลุ่มเดียว
4. แบ่ง reconnect replay เป็น bounded pages และส่งหน้าถัดไปเมื่อ cumulative ACK commit แล้วเท่านั้น
5. ตรวจ event identity, ordering, duplicate command และ restart recovery เทียบ contract ของ Phone Agent

### S2 — Concurrency, cleanup และ resource ownership

1. ตรวจ mutex/lock ordering, double unlock, concurrent answer/reject/CANCEL/BYE และ session replacement
2. call slot, SIP Call-ID map, RTP ports, TURN allocation, PeerConnection และ goroutine ต้องถูกคืนครั้งเดียว
3. live event queue เต็มต้อง fail closed ด้วย reconnect/replay ห้าม silent drop
4. รัน race detector และ cleanup/reuse regression สำหรับสายเดิมและหลาย session

### S3 — SIP interoperability hardening

1. รักษา INVITE/ACK/CANCEL/BYE, digest retry, late-200 และ final-response fallback ที่มีอยู่
2. แยก initial INVITE ออกจาก in-dialog re-INVITE อย่างชัดเจน
3. กำหนด policy สำหรับ `Require`, `100rel`/PRACK, UPDATE และ session refresh; รองรับเท่าที่ทดสอบได้
   และตอบมาตรฐานอย่างปลอดภัยเมื่อไม่รองรับ
4. SDP update ต้องยอมรับเฉพาะ codec ที่ negotiate อยู่และอัปเดต RTP/RTCP target แบบ atomic
5. malformed/duplicate/out-of-order request ต้องไม่ panic, ไม่สร้าง call ซ้ำ และไม่ค้าง resource

### S4 — Browser relay และ UI recovery

1. ตรวจ sequence/event-ID mapping เทียบ Phone Agent contract
2. transient API/network failure ต้อง retry แบบ bounded โดยไม่เปลี่ยนลำดับ
3. permanent contract conflict ต้องหยุด ACK, แสดง actionable diagnostics และ recover ได้หลังต้นเหตุถูกแก้
4. refresh/reconnect ต้องไม่ resurrect terminal call, call bar หรือ stale device/call state

### S5 — Performance และ operational evidence

1. RTP reusable-buffer path ต้องคง 0 B/op, 0 allocs/op
2. control-plane benchmark ต้องไม่ถดถอยโดยไม่มีคำอธิบายและผลเปรียบเทียบ
3. รัน unit, integration, race, vet, web build/test และ API tests
4. build Windows artifact จาก clean source หลัง automated gates ผ่าน
5. รัน pilot/load/soak จริงบน Windows Server + PBX + Edge/Chrome และเก็บ evidence ตาม acceptance matrix

## Out of scope ของรอบนี้

- codec/transcoding เพิ่มจาก PCMU/PCMA
- TLS/SRTP ระหว่าง Gateway กับ PBX
- active-active HA หรือ distributed call state
- เปลี่ยน architecture ให้ Gateway ติดต่อ Web API โดยตรง
- UI/phone feature ใหม่ที่ไม่จำเป็นต่อ defect correction

## การส่งมอบ

- source และ regression tests ใน `sip-gateway`, `web-app`, `web-api` เฉพาะส่วนที่จำเป็น
- เอกสารนี้และ DoD/evidence ledger ที่อัปเดตตามผลจริง
- benchmark comparison และผล race/vet/test
- Windows executable/package หลัง automated gates ผ่าน
- รายการ pilot gate ที่ยัง PENDING พร้อมขั้นตอนและหลักฐานที่ต้องเก็บ โดยไม่อ้างว่า production-ready ก่อนครบ

## ผลการดำเนินงาน 2026-10-19

- S1 เสร็จในขอบเขต automated: snapshot/outbox commit แบบ atomic, rollback-safe state transition,
  terminal event pair (รวม process restart), bounded replay และ replay barrier ป้องกัน live event แซงหน้า
  พร้อม delivered-watermark validation ป้องกัน cumulative ACK ข้าม event มี regression test ครบ
- S2 เสร็จในขอบเขต automated: race suite ผ่าน, terminal reserve ถูก validate ตั้งแต่ startup และ
  cleanup/reuse tests ผ่าน; live resource return-to-baseline ยังอยู่ใน soak gate
- S3 เสร็จในขอบเขต automated: รักษา INVITE/ACK/CANCEL/BYE และเพิ่ม packet-level coverage สำหรับ
  PRACK, re-INVITE, offerless re-INVITE, UPDATE, Require policy และ SDP target update
- S4 เสร็จในขอบเขต automated: Web App แยก definitive rejection ออกจาก unknown outcome,
  ตรวจ API commit proof ก่อน ACK และ rollback audio-device switch เมื่อเปลี่ยน microphone ไม่สำเร็จ
- S5 automated gates ผ่านตามหลักฐานใน DoD; Windows/PBX/browser load, soak, signing และ operational
  drill ยังเป็น release blocker ตาม pilot acceptance matrix

การเปลี่ยนแปลงทั้งหมดรักษา protocol V1, Phone Agent ingest contract, Gateway/Web API network boundary,
runtime selection และ legacy Desktop Agent path เดิม ไม่มีการเพิ่ม feature หรือ fallback behavior ใหม่
