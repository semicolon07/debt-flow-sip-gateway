# ผล implementation: Multi-profile SIP Gateway runtime

วันที่: 27/10/2026

## ขอบเขตที่ implement

- เปลี่ยน in-process SIP owner keyจาก Usernameเป็น `transport + canonical configured host + port + realm + exact username`.
- คง same-AoR idle takeoverและ active-call protectionเดิม แต่อนุญาต Usernameเดียวกันบน PBXคนละ identity.
- ใช้ opaque per-session Contact userสำหรับ inbound routingและ fallbackไป registered PBX peerเมื่อ PBXคง Usernameเดิม.
- fallbackด้วย Usernameอย่างเดียวใช้ได้เมื่อมี candidateเดียว; หลาย candidateที่แยกไม่ได้จะ fail closed.
- Canonical hostรองรับ IPv4หรือ ASCII hostnameและ reject IPv6, URL, embedded portและ invalid hostname.
- Resolverเป็น explicit injectable IPv4 A-only boundaryและ resolveใหม่ทุก REGISTER/refresh/retryโดยไม่ใช้
  SRV/NAPTR fallback. Empty, AAAA-only, cancellationและ resolver error fail closed.
- REGISTER/INVITEคง configured hostnameใน SIP Request-URI แต่ transport destinationใช้ resolved IPv4:port;
  unregisterใช้ resolved targetล่าสุดโดยไม่เปิด DNS fallbackใหม่.
- WebSocket/capability protocolยังเป็น V1และ GatewayId verificationเดิมไม่เปลี่ยน.

## หลักฐานอัตโนมัติ

```text
GOCACHE=/tmp/debt-flow-sip-gateway-go-cache go test ./...
```

ผ่านทุก packageบน macOS โดยอนุญาต loopback UDP/TCP tests. Coverageใหม่รวม composite identity,
same Usernameต่าง PBX, same-AoR active-call takeover rejection, unique Contact, PBX-peer routing,
ambiguous fallback, DNS A rotation/AAAA-only/error/cancellation, hostname Request-URIกับ explicit IPv4 destination
และ UDP/TCP paths.

## Gateที่ยังค้าง

- ทดสอบ Issabel/Asteriskจริงอย่างน้อยสอง PBXที่ใช้ extensionเดียวกันทั้ง UDP/TCP, inbound/outboundและ reconnect.
- ยืนยันว่า PBX/SBCคง opaque registered Contact URI; ถ้า rewrite Contact ต้องมี source peerที่แยกแต่ละ PBXได้.
- Windows Server signed build, load/soakและ production pilotยังเป็น release gatesเดิม.
