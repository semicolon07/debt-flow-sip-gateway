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

## Gateway or metrics unavailable

- ตรวจ Windows Service `DebtFlowSipGateway`, IIS ARR/WebSocket และ listener 8080/9090 บน loopback
- ถ้า service crash ให้เก็บ Windows Service exit code และ sanitized bundle ก่อน start ใหม่
- restart จะ terminalize call ที่ค้างด้วย `gateway_restarted`; ตรวจว่า browser relay event ไป API เพียงครั้งเดียว
- readiness 503 พร้อม gate message เป็น behavior ที่ตั้งใจไว้เมื่อ `sip.spikeApproved=false`

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
