# SIP Gateway local monitoring

ชุด monitoring แยก process จาก `sip-gateway.exe` โดยสิ้นเชิงและไม่อยู่ใน SIP/WebRTC/RTP call path
Gateway เปิดเฉพาะ read-only metrics และ diagnostics บน `127.0.0.1:9090`

## Embedded diagnostics

เปิดบน Windows Server เครื่องเดียวกัน:

```text
http://127.0.0.1:9090/diagnostics/
```

หน้าเว็บแสดง readiness, sessions, SIP registrations, calls, WSS, ICE/TURN, outbox, SQLite, CPU,
working set/RSS, virtual memory, Go heap, GC และ goroutines โดย refresh ทุก 5 วินาที ไม่มี mutation endpoint,
ไม่มีหมายเลขโทรศัพท์, SIP credential, capability หรือ user identity

CPU ของหน้า diagnostics และ Grafana ถูก normalize ตาม logical processors ที่ Go runtime ใช้งานได้
(0–100% ของ process ทั้งเครื่อง) ส่วน memory เป็นการใช้ของ process `sip-gateway.exe` ไม่ใช่ RAM รวมทั้ง Server

## Docker local stack

ใช้สำหรับเครื่องพัฒนาที่รัน Linux containers ได้:

```powershell
cd deploy\monitoring
Copy-Item .env.example .env
# แก้ GRAFANA_ADMIN_PASSWORD ใน .env ก่อนรัน
docker compose up -d
```

- Grafana: `http://127.0.0.1:3000`
- Prometheus: `http://127.0.0.1:9091`
- Alertmanager: `http://127.0.0.1:9093`

Prometheus เก็บข้อมูล 15 วันหรือสูงสุด 2 GB และแต่ละ container มี CPU/memory limit พร้อม alert CPU process
ตั้งแต่ 80% ต่อเนื่อง 10 นาที และ working set ตั้งแต่ 1.5 GiB ต่อเนื่อง 10 นาที โดย memory threshold นี้เป็น
ค่าเริ่มต้นสำหรับ pilot และต้อง tune จาก load test ของเครื่อง production

## Native Windows local stack

ดาวน์โหลดและแตก Prometheus, Alertmanager และ Grafana จากผู้ผลิต แล้วตั้งรหัสผ่านผ่าน environment:

```powershell
$env:GF_SECURITY_ADMIN_PASSWORD = "<long-random-password>"
.\scripts\start-local-monitoring.ps1 `
  -PrometheusDirectory C:\Tools\prometheus `
  -AlertmanagerDirectory C:\Tools\alertmanager `
  -GrafanaDirectory C:\Tools\grafana
```

หยุด process ที่ script เริ่มไว้:

```powershell
.\scripts\stop-local-monitoring.ps1
```

สคริปต์ native นี้เหมาะกับ local evaluation เท่านั้น Production ให้ติดตั้งแต่ละ component เป็น Windows
Service ด้วย service wrapper ที่องค์กรอนุมัติและกำหนด service identity/ACL แยกจาก `DebtFlowSipGateway`

## Network and security

- พอร์ต `3000`, `9090`, `9091`, `9093` bind ที่ loopback เท่านั้น
- หากต้องดูจากเครื่องอื่น ให้ reverse proxy เฉพาะ Grafana ผ่าน IIS HTTPS พร้อม authentication/IP allowlist
- ห้าม expose Prometheus, Alertmanager, `/metrics` หรือ embedded diagnostics ตรงสู่อินเทอร์เน็ต
- เปลี่ยน Alertmanager receiver จาก `local-ui` เป็น Teams/email/webhook ผ่าน secret-managed configuration
  ก่อน production; ห้าม commit webhook URL หรือ credential
