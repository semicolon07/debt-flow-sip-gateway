# ผลการแก้ WebRTC media continuity สำหรับ browser session อายุยาว

วันที่: 2026-10-19

## อาการและสาเหตุ

เมื่อ browser ใช้ WebRTC session เดิมโทรซ้ำหลายสาย Gateway เคยสร้าง RTP sequence number, timestamp
และ SSRC ชุดใหม่แบบสุ่มต่อ SIP call แต่ Pion ส่งทุกสายออกผ่าน WebRTC track เดิมและ rewrite เฉพาะ SSRC กับ
payload type เท่านั้น ผลคือ browser เห็น SSRC เดิมแต่ sequence/timestamp กระโดดข้าม timeline จน jitter buffer
อาจทิ้ง early media หรือเสียงสนทนาของสายใหม่ การ refresh browser สร้าง PeerConnection ใหม่จึงทำให้อาการหายชั่วคราว

## การแก้ไข

- normalize RTP sequence/timestamp ให้ต่อเนื่องตลอดอายุ WebRTC outbound track แม้เปลี่ยน SIP call
- บังคับ RTP marker ที่ packet แรกของ media source/call ใหม่ เพื่อประกาศ talkspurt boundary ให้ receiver
- รักษา forward-most boundary เมื่อมี packet reorder และรองรับ uint16/uint32 wrap-around
- รองรับ SSRC เปลี่ยนระหว่าง early media กับ answered media และ WebRTC source เปลี่ยนหลัง reconnect
- ใช้ G.711 payload length เป็น sample duration จึงรองรับ packetization 10/20/30 ms โดยไม่ hard-code 160 samples
- map RTCP ด้วย WebRTC SSRC ที่ Pion negotiate จริง ไม่ใช้ SSRC ชั่วคราวก่อน Pion rewrite
- ไม่ส่ง RFC 4733 telephone-event ผ่าน Pion track ที่ bind เป็น G.711 เพราะ Pion จะ relabel เป็น audio payload;
  DTMF จาก browser ยังคงใช้ `call.dtmf` และส่งเป็น RFC 4733 ไป PBX ตามเดิม

## กรณีที่ตรวจสอบ

- หลายสายต่อเนื่องใน browser session เดิม 500 calls / 50 packets ต่อสาย
- sequence/timestamp สุ่มคนละฐานในทุกสาย
- packet มาช้า/สลับลำดับก่อนเปลี่ยนสาย
- sequence 16-bit และ timestamp 32-bit wrap-around
- PBX เปลี่ยน SSRC ระหว่าง media legs
- browser เปลี่ยน SSRC หลัง WebRTC reconnect
- RTCP sender/report mapping ทั้ง browser → PBX และ PBX → browser
- PBX telephone-event ไม่ถูก decode เป็น G.711 audio โดย browser
- WebRTC offer negotiation คืน actual outbound SSRC ที่ไม่เป็นศูนย์

## ผลตรวจสอบ

- targeted media/WebRTC/coordinator tests: ผ่าน
- `go test -race ./...`: ผ่านทุก package โดยไม่พบ data race
- `go vet ./...`: ผ่าน
- RTP reusable-buffer hot path: 0 B/op, 0 allocs/op
- benchmark 200 interleaved streams บน Apple M2 Pro: 37.23–44.78 ns/op

ผล unit/race/benchmark ใช้ปิด code regression แต่การยืนยัน browser/PBX interoperability ต้องทดสอบซ้ำบน
Windows Server จริงด้วย browser session เดิมหลายสาย โดยไม่ refresh และเก็บ packet loss/jitter/RTCP metrics
ตาม `docs/performance-testing.md`
