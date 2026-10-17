# Windows Server 2022 deployment

## Network boundary

SIP Gateway ไม่มี route, URL, credential, client certificate หรือ HTTP client สำหรับเรียก Debt Flow Web API
โดยตรง เส้นทาง control มีเพียง browser → Web API เพื่อขอ encrypted capability และ browser → Gateway
ผ่าน WSS เพื่อนำ capability มาส่งต่อ Gateway ตรวจ AES-256-GCM ภายในเครื่อง

## Prerequisites

- Private/VPN IP และ split DNS สำหรับ `sip-gateway.uniga.app`
- IIS WebSocket Protocol, URL Rewrite และ Application Request Routing โดยเปิด proxy
- TLS certificate สำหรับ IIS port 443
- Issabel/Asterisk route มายัง SIP/RTP ports ของ server
- capability key 32 bytes ที่ provision ให้ Web API secret configuration และ Gateway เหมือนกัน
- Web API configuration: `PhoneGateway:PublicWebSocketUrl`, `PhoneGateway:GatewayId`,
  `PhoneGateway:Capability:Issuer`, `Audience`, `ActiveKeyId` และ `Keys:<keyId>`

## Install

1. Build release พร้อม release manifest, CycloneDX SBOM, checksums และ Authenticode signature:

   ```powershell
   .\scripts\build.ps1 -Version 1.0.0 `
     -CodeSigningCertificateThumbprint <certificate-thumbprint> -RequireSignature
   ```

   Development build สามารถไม่ส่ง certificate ได้ แต่ห้ามใช้เป็น pilot/release artifact
   Signed build จะปฏิเสธ source ที่ยังไม่ commit หรือ worktree ไม่ clean และ preflight จะเทียบ version ที่
   executable รายงานกับ `release-manifest.json` รวมทั้ง commit/target/dirty state

2. สร้าง key 32 bytes ใน secret manager ของผู้ดูแลและตั้งค่าฝั่ง Web API เป็น base64 จากนั้นเปิด
   elevated PowerShell บน Gateway แล้ว provision ค่าเดียวกันเข้า DPAPI LocalMachine โดยไม่ใส่ key
   ใน command history:

   ```powershell
   .\scripts\provision-capability-key.ps1 -KeyId cap-2026-10
   ```

   script จะ prompt ค่าแบบ `SecureString`, เขียนเฉพาะ DPAPI ciphertext ไปยัง
   `C:\ProgramData\DebtFlow\SipGateway\keys\cap-2026-10.dpapi` และจำกัด ACL ให้ Administrators/SYSTEM;
   หาก service ถูกลงทะเบียนแล้วจะให้ service อ่านได้ทันที มิฉะนั้น installer จะเพิ่ม read-only ACL หลัง
   SCM สร้าง virtual service account

3. Copy `dist` ไปยัง server และแก้ `config.example.yaml` ให้ `nodeId`, capability `keyId/path`,
   private/VPN IP และ PBX network ตรงกับ environment แล้วบันทึกเป็น
   `C:\ProgramData\DebtFlow\SipGateway\config.yaml`
4. Install service และ firewall จาก elevated PowerShell:

   ```powershell
   .\scripts\install-service.ps1 -BinaryPath .\sip-gateway.exe
   .\scripts\configure-firewall.ps1 -RemoteAddress 10.0.0.0/8
   ```

   installer จะหยุด service เดิมก่อน replace executable, ตั้ง service SID/ACL โดยให้เขียนได้เฉพาะ
   `data` และ `logs`, ให้ `keys` เป็น read-only แล้วรอจน service อยู่สถานะ `Running` ก่อนสำเร็จ

5. วาง `deploy\iis\web.config` ที่ IIS application root, bind hostname บน HTTPS 443 และห้าม expose
   Gateway ports 8080/9090 ออกนอก loopback
6. ตรวจ `healthz`, `readyz`, metrics และ exact-Origin WSS ผ่าน IIS แล้วทดสอบ browser → API →
   browser → Gateway capability relay; network policy ต้องพิสูจน์ว่า Gateway ออกไปหา Web API ไม่ได้

7. สร้าง preflight evidence แบบ release ซึ่งจะ fail หาก signature, binary checksum, CycloneDX SBOM,
   build info, protected-key-only config, service, probes, TLS endpoint หรือ network deny ไป Web API ไม่ผ่าน:

   ```powershell
   .\scripts\test-pilot-preflight.ps1 -RequireReleaseEvidence `
     -ForbiddenApiHost api-debt-flow.uniga.app
   ```

8. รัน SIPp matrix ทั้ง UDP/TCP และเก็บ artifacts ตาม
   `docs\pilot-acceptance-matrix.md`; ห้ามเปิด spike gate จากผล local unit test เพียงอย่างเดียว

## Sanitized diagnostics

รันจาก elevated PowerShell เมื่อต้องส่งข้อมูลให้ทีม support:

```powershell
.\scripts\collect-diagnostics.ps1 -OutputPath .\sip-gateway-diagnostics.zip
```

bundle สร้างจาก allowlist เท่านั้น: sanitized configuration metadata, service state, probe status และ
metrics ที่ชื่อขึ้นต้นด้วย allowlist โดยไม่ copy YAML, log, SQLite, capability, SIP credential, SDP,
ICE/TURN credential หรือ media ก่อน zip script จะทำ secret-pattern scan และหยุดทันทีถ้าพบข้อมูลต้องห้าม

`sip.spikeApproved` ต้องเป็น `false` จนกว่า SIPp และ Issabel/Asterisk matrix จะผ่าน Service จะ live
แต่ไม่ ready ระหว่างที่ gate ยังปิด

## Key rotation

เพิ่ม key ใหม่ทั้ง Web API secret configuration และ Gateway `protectedKeys` ก่อน เปลี่ยน
`ActiveKeyId` ฝั่ง Web API แล้วรออย่างน้อย 120 วินาทีจึงนำ key เก่าออก การ overlap ทำให้ capability
และ lease ที่ยังไม่หมดอายุใช้งานต่อได้โดยไม่สร้าง direct dependency ระหว่างสอง service

## Upgrade and rollback

Drain active calls, stop service, เก็บ `ProgramData`, เปลี่ยน signed binary, รัน `--check-config`
และ start service การ rollback ใช้ binary ก่อนหน้าและ key ring เดิม Collection runtime ต้องถูกสลับแบบ
explicit เท่านั้น ไม่มี automatic fallback ไป Desktop Agent

`uninstall-service.ps1` ลบเฉพาะ service registration และเก็บ program files, configuration, protected
keys, logs และ SQLite state ไว้ให้ผู้ดูแลกู้หรือลบเอง
