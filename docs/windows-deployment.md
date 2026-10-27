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

รายการข้างต้นเป็น **current P1 singleton deployment contract**. ใช้ singleton variablesด้านล่างต่อไปจน Web API
endpoint-registry implementationและ migration
ตาม [canonical contract](https://git.uniga.app/debt-flow/docs/-/blob/main/features/collection-sip-provider/multi-profile-architecture.md)
ถูก deployครบ ห้ามเปลี่ยน environment variablesล่วงหน้า.

เปิดใช้งานฝั่ง Web API ด้วย environment variables ด้านล่าง โดย `GatewayId`, issuer, audience, key ID
และ decoded key ต้องตรงกับ Gateway configuration ทุกค่า:

```text
PhoneGateway__Enabled=true
PhoneGateway__PublicWebSocketUrl=wss://sip-gateway.uniga.app/gateway/v1
PhoneGateway__GatewayId=sip-gateway-01
PhoneGateway__Capability__Issuer=debt-flow-web-api
PhoneGateway__Capability__Audience=debt-flow-sip-gateway
PhoneGateway__Capability__ActiveKeyId=cap-2026-10
PhoneGateway__Capability__Keys__cap-2026-10=<base64-encoded-32-byte-key>
```

ห้ามเก็บ capability key จริงใน `appsettings*.json`, source control หรือ deployment manifest ให้ inject
จาก secret manager เท่านั้น Web API จะปฏิเสธการเริ่ม process เมื่อ `PhoneGateway__Enabled=true` แต่ URL,
identity หรือ active key ตั้งค่าไม่ครบ/ไม่ถูกต้อง หากยังใช้เฉพาะ Desktop Agent ให้คง `Enabled=false`
ไว้ได้โดยไม่ต้อง provision capability key

### Planned multi-profile breaking cutover

Web APIรุ่น multi-profileไม่อ่าน singleton `PublicWebSocketUrl`/`GatewayId`และไม่มี fallback. Operationsต้อง provision
registryครบทุก environmentก่อน deploy; shared Capability ringอยู่ rootและ endpointเก็บ DisplayName/Enabled/WSS/GatewayId:

```text
PhoneGateway__Enabled=true
PhoneGateway__DefaultEndpointKey=central
PhoneGateway__Capability__Issuer=debt-flow-web-api
PhoneGateway__Capability__Audience=debt-flow-sip-gateway
PhoneGateway__Capability__ActiveKeyId=cap-2026-10
PhoneGateway__Capability__Keys__cap-2026-10=<base64-encoded-32-byte-key>
PhoneGateway__Endpoints__central__DisplayName=Central SIP Gateway
PhoneGateway__Endpoints__central__Enabled=true
PhoneGateway__Endpoints__central__PublicWebSocketUrl=wss://sip-gateway.uniga.app/gateway/v1
PhoneGateway__Endpoints__central__GatewayId=sip-gateway-01
```

Web API validate registryตอน startupและเปลี่ยน configด้วย restart ไม่มี hot reload. Gatewayทุก logical endpointได้รับ
shared verifier keysชุดเดียวกันแต่ config `gatewayId`ของตัวเอง; capabilityของ endpointอื่นต้องถูกปฏิเสธแม้ decryptได้.
Admin/browserเห็นเฉพาะ endpoint key/display name/configured statusและเลือก arbitrary URLไม่ได้.

## Install

1. Build release พร้อม release manifest, CycloneDX SBOM, checksums และ Authenticode signature:

   ```powershell
   .\scripts\build.ps1 -Version 1.0.0 -OutputDirectory .\dist\release\1.0.0 `
     -CodeSigningCertificateThumbprint <certificate-thumbprint> -RequireSignature
   ```

   Development build สามารถไม่ส่ง certificate ได้ แต่ห้ามใช้เป็น pilot/release artifact
   Signed build จะปฏิเสธ source ที่ยังไม่ commit หรือ worktree ไม่ clean และ preflight จะเทียบ version ที่
   executable รายงานกับ `release-manifest.json` รวมทั้ง commit/target/dirty state นอกจากนี้ output directory
   ของ signed build ต้องเป็น directory ใหม่หรือว่าง เพื่อไม่ให้ไฟล์ค้างจาก release ก่อนหน้าปะปนใน checksum

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
   .\scripts\install-service.ps1 -BinaryPath .\sip-gateway.exe -RequireSignature `
     -ExpectedSignerThumbprint <certificate-thumbprint>
   .\scripts\configure-firewall.ps1 -RemoteAddress 10.0.0.0/8
   ```

   installer จะหยุด service เดิมก่อน replace executable, ตั้ง service SID/ACL โดยให้เขียนได้เฉพาะ
   `data` และ `logs`, ให้ `keys` เป็น read-only แล้วรอจน service อยู่สถานะ `Running` ก่อนสำเร็จ
   โดยจะตรวจ signature/strict config จาก staged binary ก่อนหยุด service และคืน binary เดิมพร้อม start
   service กลับอัตโนมัติหาก replace, ACL, service configuration หรือ startup ขั้นใดล้มเหลว

5. วาง `deploy\iis\web.config` ที่ IIS application root, bind hostname บน HTTPS 443 และห้าม expose
   Gateway ports 8080/9090 ออกนอก loopback
6. ตรวจ `healthz`, `readyz`, metrics และ Origin allowlist WSS ผ่าน IIS แล้วทดสอบ browser → API →
   browser → Gateway capability relay; network policy ต้องพิสูจน์ว่า Gateway ออกไปหา Web API ไม่ได้

7. สร้าง preflight evidence แบบ release ซึ่งจะ fail หาก signature, binary checksum, CycloneDX SBOM,
   build info, protected-key-only config, service, probes, TLS endpoint หรือ network deny ไป Web API ไม่ผ่าน:

   ```powershell
   .\scripts\test-pilot-preflight.ps1 -RequireReleaseEvidence `
     -ForbiddenApiHost api-debt-flow.uniga.app `
     -ExpectedSignerThumbprint <certificate-thumbprint>
   ```

   checksum gate ตรวจทุกไฟล์ที่ระบุใน `SHA256SUMS` และปฏิเสธ path ที่ออกนอก release directory,
   duplicate/missing entry หรือ hash ไม่ตรง ไม่ได้ตรวจเพียง executable

8. รัน SIPp matrix ทั้ง UDP/TCP และเก็บ artifacts ตาม
   `docs\pilot-acceptance-matrix.md`; ห้ามเปิด spike gate จากผล local unit test เพียงอย่างเดียว

## Sanitized diagnostics

Operational JSON logs ออกทั้ง console และ rotating file ตาม `logging.path` ให้ใช้
`logging.level: "info"` ใน production และเปลี่ยนเป็น `"debug"` ชั่วคราวเฉพาะตอน reproduce ปัญหา
inbound/outbound จากนั้นคืนเป็น `"info"` แล้ว restart service ระดับ debug เพิ่ม sanitized SIP transaction
metadata เช่นทิศทาง, method, status, CSeq และ hashed Call-ID เท่านั้น Gateway จะไม่เขียน raw SIP,
ต้นทาง/ปลายทาง, URI/เบอร์โทร, Authorization, SDP, capability/SIP credential, ICE/TURN credential,
DTMF หรือ media content

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

เพิ่ม keyใหม่ใน shared Web API ringและ Gateway `protectedKeys`ของ **ทุก endpoint** ก่อน เปลี่ยน `ActiveKeyId`ฝั่ง
Web API แล้วรออย่างน้อย 120 วินาทีจึงนำ keyเก่าออกจากทุก verifier. ห้าม activate keyจน readinessยืนยันครบทุก endpoint.
Shared-key compromiseมี blast radiusทั้ง registry; incidentต้อง rotateทุก endpointแต่ GatewayId bindingยังคงบังคับ.

## Upgrade and rollback

Drain active calls, stop service, เก็บ `ProgramData`, เปลี่ยน signed binary, รัน `--check-config`
และ start service การ rollback ใช้ binary ก่อนหน้าและ key ring เดิม Collection runtime ต้องถูกสลับแบบ
explicit เท่านั้น ไม่มี automatic fallback ไป Desktop Agent

`uninstall-service.ps1` ลบเฉพาะ service registration และเก็บ program files, configuration, protected
keys, logs และ SQLite state ไว้ให้ผู้ดูแลกู้หรือลบเอง
