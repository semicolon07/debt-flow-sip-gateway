# SIPp acceptance scenarios

ไฟล์ในโฟลเดอร์นี้เป็น deterministic PBX-side scenarios สำหรับ gate ของ SIPGo/Asterisk profile
และต้องรันทั้ง `udp` และ `tcp` ด้วย `scripts/run-sipp-matrix.ps1` บน network เดียวกับ Gateway

- `register-digest`: REGISTER challenge, digest Authorization และ 200 OK
- `register-auth-failure`: REGISTER challenge แล้วปฏิเสธ credential ด้วย 403
- `unregister`: REGISTER ที่ `Expires: 0`; เปิด scenario แล้วสั่ง stop registration จาก Portal
- `outbound-answer-bye`: outbound provisional/answer/ACK และ remote BYE
- `outbound-answer-bye-pcma`: flow เดียวกันโดยบังคับ SDP PCMA; browser offer ต้องเลือก PCMA
- `outbound-busy`: outbound INVITE ได้ 486 และ Gateway ส่ง ACK
- `outbound-no-answer`: outbound ringing, local CANCEL, 200/487 และ ACK
- `inbound-answer-bye`: inbound INVITE ให้ browser รับสาย แล้ว remote BYE
- `inbound-answer-bye-pcma`: inbound flow เดียวกันด้วย PCMA payload type 8
- `inbound-cancel`: inbound ringing แล้ว remote CANCEL/487/ACK
- `inbound-unsupported-codec`: inbound offer ที่มีเฉพาะ Opus ต้องถูกปฏิเสธด้วย 488
- `inbound-malformed-sdp`: inbound offer ที่ connection address/port ใช้ไม่ได้ต้องถูกปฏิเสธด้วย 488
- `late-200-after-cancel`: browser วางสายระหว่าง ringing; PBX ส่ง late 200 และคาด ACK+BYE

Inbound scenarios ใช้ SIPp transaction retransmission (`retrans="500"`) เพื่อพิสูจน์ duplicate delivery
บน UDP; TCP ต้องพิสูจน์ duplicate transaction handling จาก Asterisk trace เพิ่ม เพราะ SIP transport
ไม่ทำ packet retransmission แบบ UDP

Scenario ที่เกี่ยวกับ call ต้องมี Portal session เปิดอยู่ และ operator ทำ action ตามชื่อ scenario ภายใน
240 วินาที ให้รัน `register-digest` จบก่อนแล้วเริ่ม call scenario บน PBX IP/port เดิมภายในรอบ refresh;
REGISTER และ call ใช้ Call-ID คนละ dialog จึงตั้งใจแยก process กัน ผล `exitCode=0` ของ SIPp อย่างเดียว
ไม่พอ ต้องแนบ Portal/API call-event timeline และ metrics
ก่อน/หลังตาม `docs/pilot-acceptance-matrix.md` ห้ามใช้ scenarios นี้แทน Issabel/Asterisk matrix จริง
