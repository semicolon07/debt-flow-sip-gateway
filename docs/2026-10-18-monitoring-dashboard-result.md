# SIP Gateway monitoring dashboard and alerts result

วันที่: 18/10/2026

สถานะ: **Implemented and automated verification passed**

## Isolation from the call runtime

- Embedded diagnostics เป็น read-only HTTP handler บน metrics listener `127.0.0.1:9090` เท่านั้น
- หน้า diagnostics อ่าน cached operational snapshot ที่ Gateway refresh ทุก 5 วินาที ไม่ query SQLite,
  scan call state หรือสร้าง profiler ต่อ browser refresh
- Prometheus, Alertmanager และ Grafana รันเป็นคนละ process/container ไม่อยู่ใน SIP/WebRTC/RTP call path
- local containers bind เฉพาะ loopback และจำกัด CPU/memory; Prometheus retention 15 วันหรือ 2 GB
- monitoring failure ไม่เปลี่ยน readiness, registration, session, call หรือ media lifecycle ของ Gateway

## Embedded diagnostics

URL: `http://127.0.0.1:9090/diagnostics/`

แสดงข้อมูลที่ sanitize แล้ว:

- Gateway readiness, node/version และ snapshot freshness
- authenticated/connected sessions และ capacity 250 sessions
- active calls และ capacity 200 concurrent calls
- SIP registrations, WSS connections/queue และ WebRTC PeerConnections
- ICE route, TURN allocation/quota และ durable outbox/SQLite size
- process CPU, working set/RSS, virtual memory, Go heap/system memory, GC และ goroutines

ไม่มี mutation endpoint, raw phone number, SIP credential, capability, SDP/ICE secret หรือ user identity
และตั้ง CSP, no-store, no-referrer, nosniff และ deny framing

## Grafana and alerts

- Provisioned Grafana dashboard เพิ่ม readiness, session/call capacity, SIP registrations, CPU, working set,
  Go memory, call outcomes, WSS disconnects, failed commands และ SQLite/WAL จาก dashboard เดิม
- เพิ่ม readiness, SIP registration, session/call capacity, call-command failure ratio และ WSS disconnect alerts
- เพิ่ม process CPU alert ที่ normalize ตาม processor ที่ Go runtime ใช้งานได้ และ working-set alert เริ่มต้นที่
  1.5 GiB; memory threshold ต้อง tune จาก load test ของ Windows Server production
- คง alerts เดิมสำหรับ outbox, TURN, RTP loss/jitter/bridge/source และ SQLite failure/checkpoint
- Alertmanager local receiver แสดง alert ใน UI โดยไม่มี external secret; Teams/email/webhook ต้อง inject
  ผ่าน production secret-managed configuration

## Local deployment options

- Docker Compose: Prometheus `3.13.3`, Alertmanager `0.34.1`, Grafana `12.4.0`
- Native Windows evaluation: `scripts/start-local-monitoring.ps1` และ `stop-local-monitoring.ps1`
- Production ต้องใช้ Windows service wrapper ที่องค์กรอนุมัติ พร้อม service identity/ACL แยกจาก Gateway

## Verification

- `go test ./...` ผ่านทุก package
- targeted `go test -race` ผ่าน diagnostics, observability, gateway และ deployment contracts
- `go vet ./...` ผ่าน
- Windows amd64 CGO-disabled cross-build ผ่าน
- Grafana dashboard ผ่าน JSON parsing
- Prometheus/Alertmanager/Grafana provisioning ผ่าน YAML parsing
- `docker compose config --quiet` ผ่าน; ไม่ได้ start containers เพราะ Docker daemon บนเครื่องพัฒนาไม่ทำงาน
- `git diff --check` ผ่าน

## Production follow-up

- กำหนด receiver และ escalation policy ของ Alertmanager โดยไม่ commit webhook/credential
- ติดตั้ง Grafana หลัง IIS HTTPS/authentication หากต้องเปิดดูจากเครื่องอื่น
- ทดสอบ alert simulation และปรับ threshold ด้วย baseline จาก 10 → 50 → 200 users
- เพิ่ม Windows host metrics ผ่าน windows_exporter หากต้องการ CPU/RAM/disk/network ระดับทั้งเครื่อง
  นอกเหนือจาก process metrics ของ `sip-gateway.exe`
