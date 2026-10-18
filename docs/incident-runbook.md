# SIP Gateway incident runbook

เอกสารนี้ใช้กับ P1 single-node เท่านั้น Gateway ไม่มีและต้องไม่มี route ไป Web API การ disable หรือ
rollback runtime ทำที่ Collection บน Web API/Admin Portal แบบ explicit และห้าม automatic fallback
ขณะที่มี active call

## First response

1. หยุดเปิด Collection ใหม่เข้า `sip_gateway` และเก็บเวลาที่ incident เริ่ม
2. ตรวจ `http://127.0.0.1:8080/healthz`, `/readyz` และ metrics บน `127.0.0.1:9090`
3. ดู active call/session/TURN/outbox ก่อน restart; ถ้ามี active call ให้ประกาศ drain
4. เก็บ diagnostics ด้วย `scripts\collect-diagnostics.ps1` ห้ามแนบ YAML, SQLite, raw log หรือ packet capture
   จนกว่าจะผ่านช่องทาง security-approved
5. ถ้าต้อง rollback ให้เปลี่ยน Collection เป็น `desktop_agent` แบบ explicit หลัง drain แล้วตรวจ call event
   ingest ต่อเนื่อง

## Inbound call ไม่เข้า Portal

1. ตั้ง `logging.level: "debug"` ชั่วคราวแล้ว restart service
2. ตรวจ `inbound SIP INVITE received`; ถ้าไม่พบให้ตรวจ PBX Contact, route และ Windows Firewall ก่อน
3. ถ้าพบ `inbound SIP call rejected` ให้ใช้ `reason` และ `sipStatusCode` แยกสาเหตุ เช่น
   `registration_not_ready`, `browser_session_or_webrtc_not_ready`, `webrtc_not_ready`,
   `active_call_or_session_unavailable`, `no_codec_intersection`, `invalid_sdp` หรือ `media_setup_failed`
4. เมื่อจบการตรวจให้คืน `logging.level: "info"` และ restart service

`callRef` เป็น hash สั้นของ SIP Call-ID สำหรับ correlation เท่านั้น ไม่ใช่ raw SIP Call-ID และ log จะไม่เก็บ
หมายเลขผู้โทร, raw SIP/SDP, Authorization หรือ credential

## Inbound แสดงใน Portal แต่กดรับแล้วล้ม

1. ตรวจว่ามี `inbound SIP call ringing` แล้วตามด้วย `inbound SIP call answered` และ `sipStatusCode=200`
2. ถ้าพบ `inbound SIP call answer failed` ให้ดู `failureKind` ที่ sanitize แล้ว:
   `timeout`/`ack_missing` ให้ตรวจ ACK และ NAT/firewall, `transport` ให้ตรวจ SIP UDP/TCP route,
   `dialog_id_mismatch`/`dialog_tag_missing` ให้ตรวจ PBX dialog headers
3. ถ้า browser วางสายขณะ Answer กำลังรอ ACK ระบบจะส่ง BYE หลัง ACK โดยตั้งใจ; ไม่ควรมี
   `call.connected` โผล่กลับมาหลัง `call.ended`
4. ตรวจ API timeline ว่ามี `call.created → incoming → answering → connected` หรือ terminal state
   ที่ sequence เพิ่มต่อเนื่อง ห้ามลบ outbox เพื่อข้าม event ที่ติด

Gateway รุ่นที่แก้แล้วต้องไม่แสดง `sip_response_failed` เพียงเพราะผู้ใช้กดรับหลังเห็น 180; INVITE transaction
จะถูกคงไว้จนมี final response การทดสอบ release ต้องครอบคลุมทั้ง UDP และ TCP

## Gateway or metrics unavailable

- ตรวจ Windows Service `DebtFlowSipGateway`, IIS ARR/WebSocket และ listener 8080/9090 บน loopback
- ถ้า service crash ให้เก็บ Windows Service exit code และ sanitized bundle ก่อน start ใหม่
- restart จะ terminalize call ที่ค้างด้วย `gateway_restarted`; ตรวจว่า browser relay event ไป API เพียงครั้งเดียว
- readiness 503 พร้อม gate message เป็น behavior ที่ตั้งใจไว้เมื่อ `sip.spikeApproved=false`

## SIP registration unavailable

- ตรวจ registration state ใน `http://127.0.0.1:9090/diagnostics/` และ operational log ที่ sanitize แล้ว
- ถ้ามี authenticated session แต่ registered เป็นศูนย์ ให้ตรวจ PBX reachability, SIP transport, credential,
  realm และ firewall โดยห้ามพิมพ์ password หรือ Authorization header ลง ticket
- ตรวจว่า SIP username ไม่มี session owner ซ้ำ และ browser session ยัง renew lease ตามปกติ
- ทดสอบ REGISTER หนึ่ง account หลังแก้ และรอให้ alert resolve ก่อนเปิด Collection เพิ่ม
- รุ่นที่มี registration generation guard จะ log `SIP registration failed` หรือ
  `SIP registration refresh failed` พร้อม `sessionId`, `sipStatusCode`, `failureKind` และ
  `consecutiveFailures`; ใช้ `sessionId` จับคู่หน้า diagnostics โดยไม่ต้องเปิด raw SIP log
- ถ้า extension หนึ่งได้สม่ำเสมอแต่อีก extension ได้เป็นบางครั้ง ให้เทียบ status ของ REGISTER และ INVITE
  แยกตาม `sessionId` ก่อนสรุปว่าเป็น Gateway: `401/407` challenge เป็น flow ปกติ แต่ final `403` คือ PBX
  ปฏิเสธ credential/policy/account binding; ตรวจ duplicate registration, endpoint identify/contact และ ACL
  ของ extension นั้นบน PBX
- Gateway ใช้ listener IP/port กลางร่วมกัน แต่ Contact user และ SIP Call-ID แยกต่อ session; PBX ต้องแยก
  endpoint ด้วย authentication username/Address-of-Record ไม่ควร identify หลาย extension ด้วย source IP/port
  เพียงอย่างเดียว

## Capacity pressure

- session warning/critical อยู่ที่ 80/90% ของ `maxSessions`; call warning/critical อยู่ที่ 80/90% ของ `maxCalls`
- ห้ามเพิ่ม limit ระหว่าง incident ก่อนตรวจ CPU, working set, goroutines, UDP ports, TURN allocation และ outbox
- หยุดเปิด Collection ใหม่และ drain call หาก pressure ยังเพิ่มต่อเนื่อง
- เก็บ capacity sample ก่อน/ระหว่าง/หลัง incident เพื่อยืนยันว่า memory และ goroutines กลับ baseline

## Call command failures

- alert ทำงานเมื่อมีอย่างน้อย 10 call commands ใน 5 นาทีและ failure ratio มากกว่า 10% ต่อเนื่อง 3 นาที
- แยก command type และ error code จาก sanitized log แล้วตรวจ capability, media readiness, SIP response และ outbox
- เปรียบเทียบ inbound/outbound, direct/TURN และ PBX response ก่อน restart เพื่อไม่ทำลายหลักฐาน

## Outbound ค้างที่ ringing หลังปลายทางวางสาย

- ที่ระดับ `info` ให้จับคู่ `sessionId`, `callId` และ `callRef` ตั้งแต่ `outbound SIP call started`,
  `outbound SIP call is progressing` จนถึง final response หรือ timeout; `callRef` เป็น hash และไม่มีเบอร์โทร
- ถ้ามี `outbound SIP final response received` หรือ `transport-level outbound SIP final response received`
  ระบบต้องตามด้วย terminal event และปล่อย call slot ทันที
- ถ้าไม่มี final response หลัง provisional response ระบบจะจบเป็น `no_answer` ภายใน 60 วินาทีและปล่อยให้
  โทรซ้ำได้ โดย transaction เดิมจะทำ SIP CANCEL cleanup ต่อเบื้องหลัง
- ถ้าผู้ใช้กดตัดที่ปลายทางแต่ต้องรอ timeout ให้เปิด `logging.level: "debug"` ชั่วคราว แล้วค้น
  `SIP wire message` ด้วย `callRef`: ไม่มี response ขาเข้าให้ตรวจ PBX/NAT/firewall; มี response ขาเข้าแต่ไม่มี
  application final-response log ให้เก็บ diagnostics เพื่อวิเคราะห์ transaction matching
- ห้ามเปิด raw SIP logging ใน production เพราะอาจมีหมายเลข, SDP และ Authorization; หลัง reproduce ให้คืน
  `logging.level: "info"`

## WSS disconnect spike

- alert ตัด `client_closed` และ `runtime_closed` ปกติออก แล้วตรวจ network/heartbeat/writer overflow reasons ที่เหลือ
- ตรวจ IIS WebSocket timeout, Cloudflare/VPN path, browser network และ WSS writer queue
- ถ้าเกิดพร้อม CPU/memory/call-capacity สูง ให้แก้ capacity pressure ก่อนเพิ่ม reconnect rate

## Process resource pressure

- `DebtFlowSipGatewayCPUHigh` คิด CPU แบบ normalized ด้วยจำนวน processor ที่ Go runtime ใช้งานได้; ตรวจคู่กับ
  call/session capacity, TURN relay traffic, RTP errors และ call setup latency
- `DebtFlowSipGatewayMemoryHigh` ใช้ working set 1.5 GiB เป็นค่าเริ่มต้นสำหรับ pilot เท่านั้น ต้องปรับ rule
  ให้สัมพันธ์กับ RAM และ baseline จาก load test ของ Windows Server จริงก่อน production
- ตรวจ working set, Go heap/system memory, goroutines และ GC เทียบกับก่อนเริ่มสายและหลังวางสายอย่างน้อย
  10 นาที ถ้า call count กลับศูนย์แต่ค่าเพิ่มต่อเนื่องให้เก็บ heap/goroutine profile เพื่อตรวจ leak
- ห้ามแก้ incident ด้วยการเพิ่ม call/session limit จนกว่าจะยืนยัน CPU, RAM, UDP port และ TURN headroom

## Event outbox backlog

- ตรวจ WSS connected sessions และ disconnect reason; backlog มักเกิดจาก browser ปิดหรือ relay event เข้า API ไม่สำเร็จ
- ใช้ `debt_flow_sip_gateway_outbox_capacity_ratio`; metric นี้เลือกค่าสูงสุดระหว่าง pending รวม,
  pending ต่อ session และ DB+WAL เทียบกับ limit ที่ตั้งจริงใน config
- Gateway จะไม่เรียก API เอง การแก้ต้องคืน browser/API connectivity และให้ browser ACK sequence หลัง ingest สำเร็จ
- ที่ 90% Gateway block call ใหม่แต่สงวนพื้นที่ terminal event ห้ามลบ SQLite/WAL เพื่อเคลียร์ตัวเลข
- ถ้า browser กลับมาไม่ได้ ให้ rollback Collection หลัง drain และเก็บ state ไว้เพื่อ reconciliation

## TURN pressure or denials

- แยก `authentication`, `allocation_quota` และ `bandwidth` จาก metric label
- ตรวจ ICE route; ถ้า relay เพิ่มผิดปกติให้ตรวจ VPN/firewall/UDP mux ก่อนเพิ่ม quota
- ห้ามเพิ่ม `maxAllocations`, relay port หรือ `bytesPerSecond` โดยไม่ตรวจ CPU/network/port headroom

## Media quality or bridge errors

- เทียบ direct/relay ICE route, RTP packet rate, RTCP reported loss/jitter และ bridge error direction
- `DebtFlowSipGatewayRTPPacketLossHigh` เตือนเมื่อ RTCP reported loss ratio ต่อ forwarded packets เกิน 2%
  ต่อเนื่อง 5 นาที; `DebtFlowSipGatewayRTPJitterHigh` เตือนเมื่อ jitter p95 เกิน 50 ms
- แยก label `direction`: `pbx_to_browser` สูงให้ตรวจ PBX→Gateway, RTP firewall, TURN/ICE และ browser output;
  `browser_to_pbx` สูงให้ตรวจ microphone/WebRTC uplink และ Gateway→PBX
- ตรวจ `debt_flow_sip_gateway_rtp_source_events_total` โดย `migrated` หมายถึง PBX เปลี่ยน source port
  และ Gateway เรียนรู้ port ใหม่สำเร็จ ส่วน `rejected_ip` หมายถึง source IP ไม่ตรงกับ SDP และ packet ถูกทิ้ง
- ถ้า `browser_to_pbx` เพิ่มแต่ `pbx_to_browser` คงที่ ให้ตรวจ RTP firewall, SDP `c=`/`m=audio`,
  direct media/re-INVITE และ source IP/port จริงจาก PBX ก่อน restart Gateway
- ตรวจ PBX codec ว่า PCMU/PCMA และ RFC 4733 เท่านั้น รวมถึง RTP/RTCP advertised IP/port
- packet capture อาจมีหมายเลข, SDP, ICE หรือ media จึงเป็น sensitive artifact และไม่อยู่ใน diagnostics bundle

## SQLite or disk pressure

- ตรวจ `debt_flow_sip_gateway_sqlite_failures_total` แยก `maintenance` และ `stats`; failure ใหม่เป็น
  critical เพราะอาจทำให้ retention/checkpoint หรือ capacity telemetry ไม่เป็นปัจจุบัน
- ตรวจ database/WAL bytes, outbox depth, free disk และ antivirus/file-lock policy
- ห้าม copy/edit/delete database หรือ WAL ขณะ service ทำงาน
- drain และ stop service ก่อน backup; รักษา ACL เดิมและ capability-key DPAPI files

## Recovery verification

- health และ readiness ถูกต้อง
- SIP registration กลับมา registered ผ่าน transport ที่ Collection เลือก
- WSS reconnect/replay/ACK ไม่มี duplicate terminal business event
- test call inbound/outbound ผ่าน browser และตรวจ event ใน Web API
- metric/goroutine/memory/TURN allocation/outbox กลับ baseline
