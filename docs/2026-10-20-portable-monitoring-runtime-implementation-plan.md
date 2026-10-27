# Portable monitoring runtime implementation plan

วันที่: 2026-10-20  
สถานะ: Proposed  
ขอบเขต: `sip-gateway` บน Windows Server 2022 (`windows/amd64`)

## 1. เป้าหมาย

ทำให้ผู้ดูแลสามารถใช้งาน monitoring ของ SIP Gateway ได้โดยไม่ต้องติดตั้ง Docker, WSL,
Prometheus, Alertmanager หรือ Grafana ด้วยตนเอง โดยส่งมอบเป็น portable ZIP ที่แตกไฟล์แล้วเริ่มระบบ
ได้จากคำสั่งเดียว ขณะเดียวกัน process monitoring ต้องยังแยกจาก `sip-gateway.exe` เพื่อไม่ให้ปัญหาของ
dashboard หรือ time-series storage กระทบ SIP/WebRTC/RTP call path

ผลลัพธ์ที่ต้องการ:

- มี release แบบ Gateway-only และ Gateway-with-monitoring ให้เลือก
- portable mode เริ่มและหยุด monitoring ได้ด้วย PowerShell คำสั่งเดียว โดยไม่ต้องใช้สิทธิ์ Administrator
  เมื่อรันจาก directory ที่ผู้ใช้เขียนได้
- service mode ใช้ runtime ชุดเดียวกันและเริ่มเองหลัง Windows reboot
- Grafana datasource และ SIP Gateway dashboard พร้อมใช้งานโดยไม่ต้อง configure ด้วยมือ
- ทุก endpoint bind กับ loopback เป็นค่าเริ่มต้น
- release reproducible, ระบุ version, checksum, license และ provenance ของ third-party runtime ได้
- upgrade แล้วรักษา configuration และ monitoring data เดิมไว้ และ rollback ได้

## 2. สิ่งที่ไม่ทำในแผนนี้

- ไม่ฝัง Prometheus, Alertmanager หรือ Grafana เข้าไปใน process หรือ binary ของ SIP Gateway
- ไม่สร้าง Grafana ใหม่หรือเขียน time-series database เอง
- ไม่เปิด Prometheus, Alertmanager, `/metrics` หรือ `/diagnostics` สู่อินเทอร์เน็ต
- ไม่ติดตั้ง IIS หรือเปิด remote Grafana ให้อัตโนมัติ
- ไม่บรรจุ webhook URL, SMTP credential หรือ Grafana admin password ลงใน source control/release
- ไม่ยกเลิก Docker Compose สำหรับ developer ที่ยังต้องการใช้วิธีเดิม
- ไม่รองรับ Windows ARM64 ใน milestone แรก

## 3. Architecture decision

มุมมองของผู้ใช้เป็น package และ launcher ชุดเดียว แต่ runtime ยังคงแยก process:

```text
Operator
   |
   +-- Start-DebtFlowMonitoring.ps1
           |
           +-- sip-gateway.exe       127.0.0.1:9090 (/metrics, /diagnostics)
           +-- prometheus.exe        127.0.0.1:9091
           +-- alertmanager.exe      127.0.0.1:9093
           `-- grafana-server.exe    127.0.0.1:3000
```

Prometheus และ Alertmanager เป็น standalone executable ได้ ส่วน Grafana ต้องคง distribution directory
ไว้ เพราะ `grafana-server.exe` พึ่งพาไฟล์ภายใน `conf`, `public` และ directory อื่น ๆ จึงเรียก package นี้ว่า
portable runtime ไม่ใช่ single executable

ใช้ component versions ที่ pin ไว้ใน release manifest เท่านั้น รุ่นเริ่มต้นให้ตรงกับ
`deploy/monitoring/docker-compose.yml` ณ วันที่นำแผนไป implement และการเปลี่ยน version ต้องผ่าน
dependency review และ smoke test ใหม่ทุกครั้ง

## 4. รูปแบบ release

CI สร้าง artifact สองชุดจาก source revision เดียวกัน:

```text
sip-gateway-runtime-<version>-windows-amd64.zip
sip-gateway-with-monitoring-<version>-windows-amd64.zip
```

Gateway-only มี binary, example configuration, operational scripts, documentation, SBOM, release manifest
และ checksums เหมือนเดิม ส่วน Gateway-with-monitoring เพิ่ม vendor runtime และไฟล์ที่ provision แล้ว:

```text
DebtFlow-SipGateway/
|-- app/
|   |-- sip-gateway.exe
|   `-- config.example.yaml
|-- monitoring/
|   |-- prometheus/
|   |   `-- prometheus.exe
|   |-- alertmanager/
|   |   `-- alertmanager.exe
|   `-- grafana/
|       |-- bin/grafana-server.exe
|       |-- conf/
|       `-- public/
|-- config/
|   |-- prometheus.yml
|   |-- alertmanager.yml
|   `-- grafana-provisioning/
|-- dashboards/
|   `-- sip-gateway-dashboard.json
|-- scripts/
|   |-- Start-DebtFlowMonitoring.ps1
|   |-- Stop-DebtFlowMonitoring.ps1
|   |-- Test-DebtFlowMonitoring.ps1
|   |-- Install-DebtFlowMonitoringServices.ps1
|   `-- Uninstall-DebtFlowMonitoringServices.ps1
|-- licenses/
|-- THIRD-PARTY-NOTICES.txt
|-- monitoring-components.lock.json
|-- release-manifest.json
|-- sbom.cdx.json
`-- SHA256SUMS
```

ไฟล์ที่เปลี่ยนระหว่างการใช้งานไม่เขียนทับ package directory โดยค่าเริ่มต้น:

```text
Portable mode: <package>\runtime\
Service mode:  C:\ProgramData\DebtFlow\SipGateway\monitoring\
```

ภายใต้ runtime directory แยก `generated-config`, `data`, `logs`, `run` และ `secrets` ออกจากกัน เพื่อให้
backup, retention และ ACL ทำได้ชัดเจน

## 5. Configuration model

สร้างไฟล์ `monitoring-runtime.yaml` เป็น configuration ระดับ launcher โดยมีอย่างน้อย:

- runtime/data/log directory
- Gateway metrics target; ค่าเริ่มต้น `127.0.0.1:9090`
- Grafana, Prometheus และ Alertmanager listen address/port
- Prometheus retention time และ retention size
- process resource guidance/limits ที่ตรวจสอบได้บน Windows
- startup timeout และ graceful shutdown timeout
- run mode: `portable` หรือ `service`
- optional Alertmanager receiver configuration ที่อ้างถึง secret ภายนอกเท่านั้น

launcher validate configuration และ port availability ก่อนเริ่ม process ใด ๆ จากนั้น generate vendor-specific
configuration ลง `generated-config` แบบ atomic โดยไม่แก้ไฟล์ template ใน package

ลำดับการเริ่ม:

1. Validate package manifest, executable path, config และ writable directories
2. Validate ว่า ports `3000`, `9091`, `9093` ว่าง และ Gateway metrics `9090` เข้าถึงได้หรือรายงาน warning
3. เริ่ม Alertmanager และรอ `/-/ready`
4. เริ่ม Prometheus และรอ `/-/ready`
5. เริ่ม Grafana และรอ `/api/health`
6. รัน end-to-end smoke query และพิมพ์ URL ที่เปิดใช้งานได้

ถ้า component ใดเริ่มไม่สำเร็จ launcher ต้องหยุดเฉพาะ process ที่เริ่มใน invocation นั้น คืน exit code ที่ไม่ใช่
ศูนย์ และชี้ log ที่ใช้วิเคราะห์ได้ ห้ามปล่อย partial stack ค้างโดยไม่แจ้ง

## 6. Secret และ security controls

- ทุก service bind `127.0.0.1` เป็นค่าเริ่มต้น และ template ห้ามใช้ `0.0.0.0`
- first-run สร้าง Grafana admin password แบบ cryptographically secure ถ้ายังไม่มี
- portable mode เก็บ secret ในไฟล์ที่จำกัด ACL เฉพาะผู้ใช้ปัจจุบัน; service mode ใช้ DPAPI หรือ credential
  mechanism ที่องค์กรอนุมัติและจำกัด ACL ตาม service identity
- ไม่ส่ง password ผ่าน command line เพราะ command line อาจถูกผู้ใช้อื่นอ่านได้
- log, process manifest, diagnostics bundle และ support output ต้องไม่แสดง password/webhook/credential
- ปิด anonymous access และ user sign-up ของ Grafana
- datasource ใช้ server-side access ไปยัง loopback Prometheus
- Alertmanager เริ่มด้วย local receiver ที่ไม่ส่งข้อมูลออกนอกเครื่อง จนกว่าจะ configure receiver อย่างชัดเจน
- remote access ทำผ่าน IIS HTTPS พร้อม authentication และ IP allowlist โดย expose เฉพาะ Grafana
- `Stop` ตรวจสอบทั้ง PID, executable name และ executable path ก่อน terminate เพื่อป้องกัน PID reuse
- service mode ใช้ service identity แยกจาก `DebtFlowSipGateway` และกำหนด ACL เฉพาะ directory ที่จำเป็น

## 7. Supply chain และ licensing

เพิ่ม `monitoring-components.lock.json` ที่ pin รายการต่อ component:

- component name และ version
- official download URL
- expected archive filename
- SHA-256 ของ upstream archive
- extracted executable path
- upstream project/license URL

ขั้นตอนประกอบ release ต้อง:

1. ดาวน์โหลดจาก official upstream เท่านั้นใน CI/release environment ไม่ดาวน์โหลดตอนติดตั้งที่ production
2. ตรวจ SHA-256 ก่อน extract
3. ปฏิเสธ archive ที่มี path traversal หรือ layout ไม่ตรงที่ lock file กำหนด
4. คัดลอกเฉพาะ runtime files ที่จำเป็นโดยไม่ทำลาย license/notice obligations
5. สร้าง `THIRD-PARTY-NOTICES.txt`, รวม license files และเพิ่ม component ลง CycloneDX SBOM
6. สร้าง SHA-256 สำหรับทุกไฟล์ใน artifact สุดท้าย
7. ลงลายเซ็น Authenticode ให้ executable/script ที่ DebtFlow สร้างขึ้นตาม release policy; vendor executable ต้อง
   ตรวจ signature/hash ที่ upstream รองรับและบันทึกผลไว้ใน release evidence

license และ redistribution review เป็น release gate บังคับ ก่อนเผยแพร่ package ที่รวม vendor binaries ครั้งแรก

## 8. Process lifecycle

### Portable mode

- start script ใช้ absolute path ที่ resolve จาก package root จึงรันจาก working directory ใดก็ได้
- เขียน `run/processes.json` แบบ atomic โดยบันทึก PID, executable path, start time และ package version
- start ซ้ำต้อง idempotent: ถ้า stack เดิม healthy ให้แสดงสถานะและไม่ spawn ซ้ำ
- stale process manifest ต้องตรวจพบและซ่อมได้โดยไม่ terminate process ที่ไม่ใช่ของ package
- stop แบบ graceful ก่อน และใช้ forced termination เฉพาะเมื่อ timeout พร้อมบันทึกเหตุผล
- ปิดหน้าต่าง PowerShell แล้ว process ต้องมีพฤติกรรมที่ระบุชัด: ค่าเริ่มต้นให้ process ทำงานต่อจนเรียก stop

### Service mode

- ใช้ Windows service wrapper ที่องค์กรอนุมัติและ pin version ไว้ใน lock file หรือสร้าง supervisor ที่ผ่าน
  security review; ห้ามอาศัย scheduled task เป็น production service manager
- สร้าง service แยกต่อ component เพื่อกำหนด dependency, restart policy, identity และ log ได้ชัดเจน
- service startup เป็น Automatic (Delayed Start) และไม่ทำให้ `DebtFlowSipGateway` ขึ้นต่อ monitoring
- monitoring failure ต้องไม่ restart หรือ stop Gateway
- uninstall services ไม่ลบ data/config/secrets โดยอัตโนมัติ ต้องมี `-PurgeData` ที่ขอการยืนยันแยกต่างหาก

## 9. Upgrade, rollback และ backup

- runtime binaries เป็น immutable versioned directory; ห้ามเขียนทับ version ที่กำลังรัน
- configuration/data/secrets อยู่นอก versioned binary directory
- upgrade flow: verify package -> stop monitoring -> snapshot config -> switch version -> start -> smoke test
- ถ้า smoke test ล้มเหลว ให้สลับกลับ runtime version ก่อนหน้าโดยไม่ rollback time-series data เว้นแต่ vendor
  ระบุว่า storage format ไม่ backward-compatible
- ก่อนเปลี่ยน major version ต้องทดสอบ Grafana database และ Prometheus TSDB compatibility
- เอกสาร backup ต้องครอบคลุม Grafana data, generated configuration, secrets และ user-created dashboards;
  Prometheus TSDB backup ให้ใช้วิธีที่ upstream รองรับ ไม่ copy directory ขณะมีการเขียนแบบไม่ควบคุม
- release manifest บันทึก Gateway version และ monitoring component versions ที่ทำงานร่วมกันแล้ว

## 10. งานที่ต้อง implement

### Phase 1 — Package specification และ dependency locking

- เพิ่ม `deploy/monitoring/monitoring-components.lock.json`
- เพิ่ม schema validation/test สำหรับ lock file
- ทำ license/redistribution review และกำหนด files ที่ต้องรวมต่อ component
- ปรับ `scripts/build.ps1` ให้สร้าง artifact Gateway-only โดยไม่เปลี่ยน contract เดิม
- เพิ่ม `scripts/build-monitoring-bundle.ps1` สำหรับดาวน์โหลด, verify, extract และ assemble bundle
- เพิ่ม offline build mode ที่รับ pre-populated vendor cache แต่ยังตรวจ checksum เหมือนเดิม

ผลสำเร็จ: CI สร้าง portable directory ที่ deterministic และ tampered archive ถูก reject

### Phase 2 — Portable launcher

- refactor `scripts/start-local-monitoring.ps1` ให้ reusable และไม่ต้องรับ vendor directories เมื่อรันจาก bundle
- เพิ่ม `Start-DebtFlowMonitoring.ps1`, `Stop-DebtFlowMonitoring.ps1` และ status helper
- เพิ่ม configuration validation, port validation, readiness polling, rollback-on-start-failure และ structured logs
- แก้ stop logic ให้ตรวจ executable path/start time และรองรับ stale manifest
- generate Prometheus/Grafana config จาก template ลง runtime directory โดยไม่ mutate package
- provision datasource, dashboard และ alert rules อัตโนมัติ

ผลสำเร็จ: clean Windows Server สามารถแตก ZIP และเริ่ม/หยุด stack ได้โดยไม่ติดตั้ง Docker หรือ vendor tools

### Phase 3 — First-run security และ diagnostics

- เพิ่ม secure first-run Grafana credential flow
- เพิ่ม ACL setup สำหรับ portable/service mode และ test ว่า non-authorized user อ่าน secret ไม่ได้
- เพิ่ม `Test-DebtFlowMonitoring.ps1` ตรวจ process, readiness, scrape target, rule loading และ Grafana health
- เพิ่ม sanitized support bundle ที่รวม versions/config validation/log tail แต่ตัด secrets
- เพิ่ม failure messages ที่ระบุ component, log path และ remediation

ผลสำเร็จ: ไม่มี default credential, ไม่มี secret ใน command line/log และ operator วินิจฉัย startup failure ได้

### Phase 4 — Windows Services

- เลือกและอนุมัติ service wrapper/supervisor พร้อม dependency lock และ license review
- เพิ่ม install/uninstall/upgrade scripts ที่ idempotent
- สร้าง service identities, directory ACLs, dependency order และ restart policy
- เพิ่ม reboot/recovery tests และยืนยันว่า monitoring failure ไม่กระทบ Gateway

ผลสำเร็จ: production node กลับมาเก็บ metrics และเปิด Grafana ได้เองหลัง reboot โดยไม่ต้อง login

### Phase 5 — CI, release และ operational documentation

- เพิ่ม Windows bundle build job และเก็บ artifacts ทั้งสองชนิด
- เพิ่ม PowerShell lint/parse tests และ Pester tests สำหรับ launcher/config generation
- เพิ่ม malware/signature/hash verification ตาม release policy
- เพิ่ม clean-VM smoke test และ upgrade/rollback test
- ปรับ `docs/local-monitoring.md` ให้ portable bundle เป็นเส้นทางหลักสำหรับ single-node deployment
- เพิ่ม operator runbook: install, start/stop/status, reset credential, backup, upgrade, rollback และ uninstall
- คง Docker Compose ไว้เป็น developer option และทดสอบ dashboard/rules จาก source เดียวกับ portable stack

ผลสำเร็จ: release evidence แสดงผล build, signature, checksum, license, clean install, reboot และ rollback

## 11. ไฟล์หลักที่คาดว่าจะเปลี่ยนหรือเพิ่ม

```text
scripts/build.ps1
scripts/build-monitoring-bundle.ps1                 (new)
scripts/Start-DebtFlowMonitoring.ps1               (new)
scripts/Stop-DebtFlowMonitoring.ps1                (new)
scripts/Test-DebtFlowMonitoring.ps1                (new)
scripts/Install-DebtFlowMonitoringServices.ps1     (new)
scripts/Uninstall-DebtFlowMonitoringServices.ps1   (new)
deploy/monitoring/monitoring-components.lock.json  (new)
deploy/monitoring/monitoring-runtime.example.yaml  (new)
deploy/monitoring/templates/*                       (new/refactor)
docs/local-monitoring.md
docs/portable-monitoring-operations.md              (new)
.github/workflows/ci.yml
```

ชื่อ script ใช้ PascalCase สำหรับ operator-facing entry points ส่วน script เดิมคงเป็น compatibility shim
อย่างน้อยหนึ่ง release cycle แล้วแจ้ง deprecation ก่อนนำออก

## 12. Test matrix

| กลุ่ม | กรณีทดสอบสำคัญ |
|---|---|
| Build | upstream checksum ถูก/ผิด, cache online/offline, archive layout ผิด, deterministic manifest |
| Clean install | Windows Server 2022 ที่ไม่มี Docker/Grafana/Prometheus, path มีช่องว่าง, non-admin portable mode |
| Startup | ทุก port ว่าง, port ชน, Gateway ยังไม่ขึ้น, component crash, readiness timeout |
| Lifecycle | start ซ้ำ, stop ซ้ำ, stale PID, PID reuse, PowerShell window ปิด, orphan process |
| Configuration | custom runtime path/ports/retention, invalid YAML, read-only package directory |
| Security | loopback binding, Grafana authentication, secret ACL, secret redaction, non-admin access |
| Function | Prometheus target UP, rules loaded, Grafana datasource healthy, dashboard มีข้อมูลจริง |
| Resilience | Prometheus/Grafana/Alertmanager crash แยกกัน และ Gateway calls ไม่สะดุด |
| Storage | retention time/size, disk full warning, restart หลัง unclean shutdown |
| Service | install/reinstall/uninstall, delayed start, reboot, service recovery, identity/ACL |
| Upgrade | same version, patch/major component upgrade, rollback, config/data preservation |
| Compatibility | Docker และ portable ใช้ dashboard/rules semantics เดียวกัน |

## 13. Acceptance criteria

ถือว่า milestone portable runtime พร้อมใช้งานเมื่อผ่านครบทุกข้อ:

1. เครื่อง Windows Server 2022 สะอาดแตก artifact และเริ่ม monitoring ได้ด้วยหนึ่งคำสั่งโดยไม่ติดตั้ง Docker
2. ไม่มี network download ระหว่าง install/start บน production node
3. Grafana, Prometheus และ Alertmanager bind เฉพาะ loopback และ Grafana บังคับ authentication
4. `Test-DebtFlowMonitoring.ps1` ยืนยัน target UP, rules loaded, dashboard datasource healthy และคืน exit code
   ที่ automation ใช้ได้
5. start/stop/status idempotent และไม่ terminate process อื่นเมื่อ PID file ค้างหรือ PID ถูก reuse
6. component monitoring ล่มหรือ restart แล้ว active SIP call และ Gateway process ไม่ถูกหยุด
7. restart เครื่องแล้ว service mode กลับมา healthy โดยไม่ต้อง login
8. upgrade และ rollback รักษา config, secrets, Grafana data และ Prometheus data ตาม compatibility policy
9. artifact มี SBOM, third-party notices, upstream checksums, release manifest และ `SHA256SUMS` ครบ
10. clean-VM, security, reboot และ rollback evidence ถูกแนบกับ release candidate

## 14. Rollout

1. **Development:** ใช้ portable mode บน Windows VM และเปรียบเทียบ queries/dashboard กับ Docker stack
2. **Internal pilot:** เปิดใช้บน SIP Gateway หนึ่ง node เก็บ resource baseline อย่างน้อย 7 วัน และตรวจ disk growth
3. **Production pilot:** ติดตั้ง service mode หนึ่ง node พร้อม synthetic check และทดสอบ reboot/rollback ใน maintenance window
4. **General rollout:** กระจาย signed artifact และ runbook หลังผ่าน pilot; คง Gateway-only package สำหรับ node ที่ใช้
   central monitoring

ระหว่าง pilot ให้เก็บ CPU/RSS ของแต่ละ monitoring process, scrape duration/failures, TSDB size growth และ Grafana
response time แล้วปรับ retention/resource guidance ก่อน rollout ทั้งหมด

## 15. Open decisions ก่อนเริ่ม Phase 4

- service wrapper/supervisor ตัวใดผ่านมาตรฐาน security และ license ขององค์กร
- service accounts ใช้ local virtual accounts หรือ managed account รูปแบบใด
- Grafana remote access จะใช้ IIS authentication แบบใด
- production alert receiver เป็น Teams, email หรือ webhook และ secret จะมาจากระบบใด
- release retention และ support window ของ monitoring component versions

Open decisions เหล่านี้ไม่ขวาง Phase 1–3 และ portable evaluation แต่ต้องปิดก่อนประกาศ service mode เป็น production-ready
