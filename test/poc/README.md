# SIP Gateway browser POC

POC นี้ทดสอบ WSS protocol v1, WebRTC audio/ICE/TURN, SIP registration, inbound/outbound call,
DTMF และ durable event ACK โดยไม่ต้องรัน Angular Web App

## 1. ตั้งค่า Gateway สำหรับ local POC

คัดลอก `config.example.yaml` เป็น config development แล้วเปลี่ยนเฉพาะค่าที่จำเป็น รวมถึง:

```yaml
http:
  listen: "127.0.0.1:8080"
  metricsListen: "127.0.0.1:9090"
  allowedOrigins:
    - "http://127.0.0.1:8765"
  allowInsecureLoopbackOrigin: true
  authTimeout: 5s
  heartbeat: 15s
  idleTimeout: 45s
```

ตั้ง `sip.spikeApproved: true` เฉพาะเมื่อกำลังทดสอบกับ SIPp หรือ PBX ที่ตั้งใจไว้ และกำหนด
`ice.advertisedIp`, `turn.advertisedIp`, SIP/RTP ports รวมถึง capability key ให้ตรงกับ environment

`allowInsecureLoopbackOrigin` ใช้ได้เฉพาะ HTTP loopback และ production preflight จะ fail หากเป็น `true`

## 2. รัน Gateway และ POC server

จาก repository root:

```powershell
go run ./cmd/sip-gateway -config .\config.poc.yaml
```

เปิด terminal อีกหน้าหนึ่ง:

```powershell
go run ./tools/poc-server
```

แล้วเปิด `http://127.0.0.1:8765/sip-gateway-poc.html` ใน Edge หรือ Chrome
หน้า loopback ถือเป็น secure context สำหรับ microphone ใน browser รุ่นที่รองรับ

## 3. ทดสอบแบบ Standalone โดยไม่ใช้ Web API

ใช้ `dist/config.windows-poc.yaml` และให้ `nodeId` ตรงกับช่อง **Gateway ID** บนหน้า POC จากนั้น:

1. เว้น API base URL และ bearer token ว่างไว้
2. กรอก PBX IP/hostname, port, SIP username และ password แบบเดียวกับ POC ของ SIP Agent เดิม
3. ค่า transport, realm, Gateway ID และ development key อยู่ใน Advanced โดยใส่ค่า POC ไว้ให้แล้ว
4. กด **Configure SIP + สร้าง capability** แล้วกด **Connect + WebRTC**
5. เมื่อ WebRTC พร้อม ให้กด **REGISTER**
6. กรอก destination แล้วกด **Start outbound call** ได้เลย หน้า POC จะสร้าง call context/capability ให้อัตโนมัติ
   และจะแปลงเบอร์ธรรมดาเป็น `sip:<เบอร์>@<PBX host>:<port>`; ปุ่มสร้าง capability แยกมีไว้สำหรับตรวจสอบแบบ manual
   ทุกความพยายามโทรจะใช้ Call ID ใหม่ เพื่อไม่ชน durable event จากความพยายามก่อนหน้า
   ปุ่มโทรจะรอ WebRTC เชื่อมต่อจริงสูงสุด 30 วินาทีก่อนส่ง `call.start` เพื่อไม่ให้เริ่ม RTP bridge ก่อน media negotiation พร้อม

POC รองรับ inbound call เช่นกัน โดย PBX ต้อง route extension มาที่ SIP Contact ของ Gateway และต้องเข้าถึง
Gateway ที่ `sip.rtpAdvertisedIp:sip.listenPort` ผ่าน transport ที่ตั้งไว้ เมื่อ WSS, WebRTC และ SIP เป็นสีเขียว
ให้โทรเข้า extension นั้น POC จะแสดง caller, เล่น ringtone และเปิดปุ่ม **Answer inbound/Reject inbound**
อัตโนมัติ หากไม่มีเสียง ให้ตรวจว่าเลือก **Local ringtone / ringback**, browser ไม่ได้ mute tab, Windows มี
output audio device และ browser ได้รับ user gesture จากปุ่ม Connect แล้ว

หากเครื่องหรือ RDP session ไม่มี microphone ให้เลือก **No microphone / Silent audio** ก่อน Connect
POC จะสร้าง audio track ที่ไม่มีเสียงเพื่อให้ WebRTC negotiation เดินต่อได้ เหมาะสำหรับทดสอบ REGISTER,
โทรออก/รับสาย, DTMF และ hangup แต่ใช้สนทนาจริงไม่ได้
บน Windows Server โหมดนี้จะไม่รอให้ระบบมี audio endpoint ก่อนสร้าง SDP offer
หาก browser ตอบ `NotFoundError` หน้า POC จะสลับเป็น Silent audio และเริ่ม SDP negotiation ต่อให้อัตโนมัติ

Standalone mode สร้าง AES-256-GCM capability ใน browser ตาม protocol เดียวกับ Gateway โดยตรง
จึงไม่ต้องเรียก Web API แต่เป็น development path เท่านั้น ห้ามนำ plaintext capability key ไปใช้ production
SIP password และ key อยู่เฉพาะใน memory ของ tab และไม่ถูกเขียนลง browser storage หรือ sanitized log

ค่าใน package ตั้ง hard limit ไว้ที่ 250 authenticated sessions และ 200 concurrent calls แต่ POC หนึ่ง tab
ใช้ทดสอบได้หนึ่ง SIP identity/หนึ่ง active call เท่านั้น การยืนยัน 200 calls ต้องใช้ load driver กับ PBX/SIPp
และ Windows host จริงตาม `docs/performance-testing.md`; การเปิดหน้า POC จำนวนมากไม่ใช่ benchmark ที่เชื่อถือได้

## 4. ขอ capability ผ่าน Web API

วิธีแนะนำคือกรอก API base URL (รวม `/v1`), Collection ID และ bearer token หรือใช้ auth cookie
จากนั้นกด **ขอ capability จาก API** หาก local API ไม่อนุญาต CORS ให้ขอ capability ด้วย Portal/curl
แล้ว paste เฉพาะค่า session/call capability ลงในช่องที่เตรียมไว้

Session capability และ call capability ใช้ได้ครั้งเดียวและมี TTL สั้น หาก connect/call ไม่สำเร็จต้องขอใหม่
POC ไม่เขียน bearer token หรือ capability ลง `localStorage`, `sessionStorage`, URL หรือ sanitized log

## 5. ลำดับทดสอบผ่าน Web API

1. ขอ session capability แล้วกด **Connect + WebRTC** และอนุญาต microphone
2. รอ WSS/WebRTC status พร้อม แล้วกด **REGISTER**
3. กรอก destination และกด **ขอ call capability จาก API**
4. กด **Start outbound call** หรือรอสายเข้าแล้วใช้ Answer/Reject
5. ทดสอบ DTMF, Hangup, microphone switching และ `Force TURN relay`
6. ตรวจ selected ICE route, event sequence และ ACK ใน sanitized event log

POC นี้ไม่แทน Web App event ingest acceptance test เพราะ ACK ถูกส่งตรงหลังรับ event โดยไม่ได้ ingest
เข้า Web API ก่อน ใช้ Portal จริงสำหรับ `EVT-01` exactly-once verification
