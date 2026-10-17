# SIP Gateway performance and capacity testing

Unit tests ตรวจ correctness และ race ได้ แต่ไม่สามารถให้ตัวเลข production CPU/memory ที่เชื่อถือได้
เพราะไม่มี browser DTLS-SRTP, ICE/TURN, Windows networking, IIS และ PBX จริง การวัด P1 แบ่งเป็นสามชั้น
และห้ามนำผลคนละชั้นมาสรุปแทนกัน

## 1. Repeatable Go benchmarks

บน Windows host เดียวกันและ power plan เดียวกัน ให้รัน:

```powershell
.\scripts\run-local-benchmarks.ps1 -Count 5 -Benchtime 2s
```

ผลรายงาน `ns/op`, throughput, `B/op` และ `allocs/op` สำหรับ capability AES-GCM, WSS JSON decode,
RTP rewrite ทั้ง single stream/200 concurrent calls, runtime snapshot ที่ 250 sessions/200 calls และ SQLite
outbox append+ACK ตัวเลขนี้เหมาะกับ regression comparison ระหว่าง commit ไม่ใช่ server sizing

ตัวอย่าง smoke baseline วันที่ 2026-10-17 บน Darwin/arm64, Apple M2 Pro, `-benchtime=200ms -count=1`:

| benchmark | time/op | bytes/op | allocs/op |
|---|---:|---:|---:|
| decode + validate `call.start` | 2.542 us | 2,867 | 22 |
| verify AES-GCM session capability | 3.818 us | 4,999 | 23 |
| RTP header/payload translate | 63.75 ns | 304 | 2 |
| RTP translate, 200 interleaved call streams | 64.28 ns | 304 | 2 |
| snapshot, 250 sessions / 200 active calls | 11.923 us | 2,352 | 4 |
| SQLite outbox append + ACK | 731.145 us | 4,635 | 99 |

ผลนี้ยืนยันว่า benchmark harness ทำงานและใช้เป็นจุดอ้างอิงในการหา regression เท่านั้น ไม่รวม WSS/TLS,
DTLS-SRTP, ICE/TURN, network I/O, SIP PBX I/O หรือ Windows scheduler จึงห้ามแปลงเป็นจำนวนสายต่อเครื่อง

สำหรับ packetization 20 ms ที่ P1 advertise นั้น หนึ่งสาย G.711 จะมีประมาณ 50 RTP packets/s ต่อทิศทาง
ดังนั้น 200 สาย full-duplex คือประมาณ 20,000 packet translations/s และ audio payload รวมประมาณ 3.2 MB/s
ก่อนนับ RTP/UDP/IP/SRTP/TURN overhead จาก benchmark ปัจจุบัน translation path สร้าง allocation 304 bytes/packet
หรือประมาณ 6.1 MB/s ที่ workload นี้ ค่านี้เป็น lower-bound model สำหรับตามหา GC/allocation regression ไม่ใช่
ค่าทดแทน live network measurement

## 2. Live load/soak observation

ติดตั้ง signed release บน clean Windows Server 2022 แล้วเริ่ม sampler ก่อนสร้าง workload:

```powershell
.\scripts\observe-live-capacity.ps1 -DurationMinutes 120 -SampleSeconds 5 `
  -OutputDirectory .\pilot-evidence\<run-id>\metrics
```

ระหว่าง sampler ทำงาน ให้ workload driver สร้าง 250 authenticated/registered browser sessions,
คง 200 established calls, ทำอย่างน้อย 400 completed calls และบังคับทดสอบทั้ง direct ICE กับ TURN
sampler เก็บ CPU normalized ต่อ logical processor, working/private memory, handles, threads, TCP/UDP
resources, Go heap/goroutines และ Gateway counters โดยไม่เก็บ capability, SIP/SDP หรือ media

`cpuPercent` เป็นสัดส่วน CPU ทั้งเครื่องหลังหารด้วยจำนวน logical processors (ช่วงปกติ 0-100)
ส่วน `capacity-summary.json` เก็บ high-water mark ของ CPU, memory, sessions, registrations, calls,
PeerConnections, TURN allocations, sockets, goroutines, outbox depth และ outbox capacity ratio เพื่อยืนยันว่า
workload ที่ตั้งใจถูกสร้างจริง

## 3. Acceptance interpretation

รายงานต้องมีช่วง baseline ก่อน load, steady state, cleanup และอย่างน้อย 10 นาทีหลังจบ workload เปรียบเทียบ:

- CPU steady-state และ spike ต่อ established call
- working set/Go heap ต่อ session และต่อ call
- goroutine, handle, socket, RTP port และ TURN allocation high-water mark
- call setup p50/p95, error rate, packet loss/jitter และ TURN bandwidth denial
- outbox depth และ SQLite/WAL growth
- หลัง cleanup counters/resources ต้องกลับสู่ baseline band ที่ reviewer ตกลงไว้

Production target ระบุ capacity floor 250 sessions/200 calls แต่ยังไม่กำหนด CPU/RAM guarantee ก่อนมีผลบน target VM
หาก resource ไม่กลับ baseline หรือ error/crash ใดเกิดขึ้น ให้ถือ gate `LOAD-01/SOAK-01` เป็น FAIL
