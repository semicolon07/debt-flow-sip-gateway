const byId = (id) => document.getElementById(id);
const number = new Intl.NumberFormat('th-TH');
let previousCPU = null;

function bytes(value) {
  if (!Number.isFinite(value) || value <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB'];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

function capacity(current, maximum, meterId, labelId) {
  const percent = maximum > 0 ? Math.min(100, (current / maximum) * 100) : 0;
  byId(meterId).value = percent;
  byId(labelId).textContent = `${percent.toFixed(0)}%`;
}

function rows(elementId, values, emptyColumns) {
  const body = byId(elementId);
  body.replaceChildren();
  if (values.length === 0) {
    const row = document.createElement('tr');
    const cell = document.createElement('td');
    cell.colSpan = emptyColumns;
    cell.className = 'muted';
    cell.textContent = 'ไม่มีข้อมูลที่กำลังใช้งาน';
    row.append(cell);
    body.append(row);
    return;
  }
  for (const valuesForRow of values) {
    const row = document.createElement('tr');
    for (const value of valuesForRow) {
      const cell = document.createElement('td');
      cell.textContent = String(value);
      row.append(cell);
    }
    body.append(row);
  }
}

function render(data) {
  const activeCalls = data.runtime.calls.reduce((total, call) => total + call.count, 0);
  const registered = data.runtime.registrations.registered || 0;
  const sampleTime = new Date(data.generatedAtUtc).getTime() / 1000;
  if (previousCPU && sampleTime > previousCPU.at && data.process.cpuSeconds >= previousCPU.seconds) {
    const cores = Math.max(1, data.process.cpuCores || 1);
    const used = ((data.process.cpuSeconds - previousCPU.seconds) / (sampleTime - previousCPU.at) / cores) * 100;
    byId('cpu').textContent = `${used.toFixed(1)}% (${data.process.cpuCores} logical CPUs)`;
  }
  previousCPU = { seconds: data.process.cpuSeconds, at: sampleTime };
  byId('identity').textContent = `${data.nodeId} · ${data.version}`;
  byId('status').textContent = data.ready ? 'พร้อมใช้งาน' : 'ยังไม่พร้อม';
  byId('status').className = `badge ${data.ready ? 'ready' : 'failed'}`;
  byId('freshness').textContent = new Date(data.generatedAtUtc).toLocaleTimeString('th-TH');
  byId('sessions').textContent = number.format(data.runtime.connectedSessions);
  byId('sessions-detail').textContent = `${number.format(data.runtime.authenticatedSessions)} authenticated / ${number.format(data.limits.sessions)} max`;
  byId('calls').textContent = number.format(activeCalls);
  byId('calls-detail').textContent = `${number.format(data.limits.calls)} concurrent-call limit`;
  byId('registered').textContent = number.format(registered);
  byId('registration-detail').textContent = Object.entries(data.runtime.registrations).map(([key, value]) => `${key} ${value}`).join(' · ') || 'ไม่มี session';
  byId('peers').textContent = number.format(data.infrastructure.peerConnections);
  byId('wss-detail').textContent = `${number.format(data.infrastructure.wssConnections)} WSS · queue ${number.format(data.infrastructure.wssWriterQueue)}`;
  byId('turn').textContent = number.format(data.turn.allocations);
  byId('turn-detail').textContent = `${number.format(data.turn.allocationLimit)} allocation limit`;
  byId('outbox').textContent = number.format(data.storage.pendingEvents);
  byId('storage-detail').textContent = `${data.storage.healthy ? 'storage healthy' : 'storage unavailable'} · DB ${bytes(data.storage.databaseBytes)} · WAL ${bytes(data.storage.walBytes)}`;
  byId('rss').textContent = bytes(data.process.residentMemoryBytes);
  byId('heap').textContent = bytes(data.process.heapBytes);
  byId('heap-in-use').textContent = bytes(data.process.heapInUseBytes);
  byId('system-memory').textContent = bytes(data.process.systemBytes);
  byId('virtual-memory').textContent = bytes(data.process.virtualMemoryBytes);
  byId('goroutines').textContent = number.format(data.process.goroutines);
  byId('gc-count').textContent = number.format(data.process.gcCount);
  capacity(data.runtime.authenticatedSessions, data.limits.sessions, 'sessions-meter', 'sessions-percent');
  capacity(activeCalls, data.limits.calls, 'calls-meter', 'calls-percent');
  capacity(data.turn.allocations, data.turn.allocationLimit, 'turn-meter', 'turn-percent');
  rows('call-rows', data.runtime.calls.map((call) => [call.direction, call.state, number.format(call.count)]), 3);
  rows('ice-rows', Object.entries(data.infrastructure.iceRoutes).map(([route, count]) => [route, number.format(count)]), 2);
  byId('error').classList.add('hidden');
}

async function refresh() {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 4000);
  try {
    const response = await fetch('/diagnostics/api/v1/snapshot', { cache: 'no-store', signal: controller.signal });
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    render(await response.json());
  } catch (error) {
    byId('status').textContent = 'เชื่อมต่อไม่ได้';
    byId('status').className = 'badge failed';
    byId('error').textContent = `อ่าน diagnostics ไม่สำเร็จ: ${error instanceof Error ? error.message : 'unknown error'}`;
    byId('error').classList.remove('hidden');
  } finally {
    clearTimeout(timeout);
  }
}

refresh();
setInterval(refresh, 5000);
