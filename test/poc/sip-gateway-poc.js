'use strict';

const $ = (id) => document.getElementById(id);
const state = {
  socket: null,
  peer: null,
  localStream: null,
  remoteStream: null,
  silentAudioContext: null,
  silentAudioSource: null,
  pendingCandidates: [],
  localCandidates: [],
  offerSent: false,
  pendingCommands: new Map(),
  renewTimer: null,
  statsTimer: null,
  lastSequence: 0,
  registrationState: 'unknown',
  callState: 'idle',
  callDirection: '',
  toneContext: null,
  toneOscillators: [],
  toneTimers: [],
  toneGain: null,
  activeTone: '',
  toneGeneration: 0,
  logEntries: [],
  standalone: null,
};

const sensitiveKey = /capability|password|credential|authorization|token|sdp|candidate|destination|remoteParty|digit/i;

function uuid() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = [...bytes].map((value) => value.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function base64URL(bytes) {
  let binary = '';
  for (const value of bytes) binary += String.fromCharCode(value);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function decodeCapabilityKey(value) {
  const normalized = value.trim().replace(/-/g, '+').replace(/_/g, '/');
  const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, '=');
  let binary;
  try {
    binary = atob(padded);
  } catch {
    throw new Error('Capability key ต้องเป็น base64 ที่ถูกต้อง');
  }
  const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
  if (bytes.length !== 32) throw new Error('Capability key ต้องมีขนาด 32 bytes');
  return bytes;
}

async function sealCapability(claims) {
  const keyId = $('standaloneKeyId').value.trim();
  if (!keyId) throw new Error('กรุณากรอก capability key ID');
  const rawKey = decodeCapabilityKey($('standaloneKey').value);
  try {
    const encodedKeyId = base64URL(new TextEncoder().encode(keyId));
    const aad = new TextEncoder().encode(`dfcap1.${encodedKeyId}`);
    const nonce = crypto.getRandomValues(new Uint8Array(12));
    const key = await crypto.subtle.importKey('raw', rawKey, { name: 'AES-GCM' }, false, ['encrypt']);
    const sealed = new Uint8Array(await crypto.subtle.encrypt(
      { name: 'AES-GCM', iv: nonce, additionalData: aad, tagLength: 128 },
      key,
      new TextEncoder().encode(JSON.stringify(claims)),
    ));
    const ciphertext = sealed.slice(0, -16);
    const tag = sealed.slice(-16);
    return `dfcap1.${encodedKeyId}.${base64URL(nonce)}.${base64URL(ciphertext)}.${base64URL(tag)}`;
  } finally {
    rawKey.fill(0);
  }
}

function sanitize(value, depth = 0) {
  if (depth > 5) return '[truncated]';
  if (Array.isArray(value)) return value.slice(0, 20).map((item) => sanitize(item, depth + 1));
  if (!value || typeof value !== 'object') return value;
  const result = {};
  for (const [key, item] of Object.entries(value)) {
    result[key] = sensitiveKey.test(key) ? '[redacted]' : sanitize(item, depth + 1);
  }
  return result;
}

function log(label, value = null, level = 'info') {
  const entry = {
    at: new Date().toISOString(),
    level,
    label,
    ...(value === null ? {} : { value: sanitize(value) }),
  };
  state.logEntries.unshift(entry);
  state.logEntries.length = Math.min(state.logEntries.length, 400);
  $('log').textContent = state.logEntries.map((item) => JSON.stringify(item)).join('\n');
}

function setPill(id, text, tone = '') {
  const element = $(id);
  element.textContent = text;
  element.className = `pill ${tone}`.trim();
}

function newIdentity() {
  $('agentInstanceId').value = uuid();
  $('agentSessionId').value = uuid();
  $('callId').value = uuid();
  $('callContextId').value = '';
  $('callCapability').value = '';
  $('activeCallId').value = '';
  $('remoteParty').value = '';
  state.lastSequence = 0;
  state.registrationState = 'unknown';
  setCallState('idle', '');
  state.standalone = null;
  log('New ephemeral browser identity created');
}

function apiURL(path) {
  const base = $('apiBase').value.trim().replace(/\/+$/, '');
  if (!base) throw new Error('กรุณากรอก API base URL');
  return `${base}/${path.replace(/^\/+/, '')}`;
}

async function apiPost(path, body) {
  const headers = { 'content-type': 'application/json' };
  const bearer = $('bearerToken').value.trim();
  if (bearer) headers.authorization = `Bearer ${bearer}`;
  const response = await fetch(apiURL(path), {
    method: 'POST',
    credentials: 'include',
    cache: 'no-store',
    headers,
    body: JSON.stringify(body),
  });
  if (!response.ok) throw new Error(`API ตอบ ${response.status} ${response.statusText}`);
  const envelope = await response.json();
  if (!envelope || typeof envelope !== 'object' || !envelope.data) throw new Error('รูปแบบ API response ไม่ถูกต้อง');
  return envelope.data;
}

async function requestSessionCapability() {
  const collectionId = $('collectionId').value.trim();
  if (!collectionId) throw new Error('กรุณากรอก Collection ID');
  const data = await apiPost(`portal/collections/${encodeURIComponent(collectionId)}/phone-gateway/sessions`, {
    agentInstanceId: $('agentInstanceId').value.trim(),
    agentSessionId: $('agentSessionId').value.trim(),
    portalVersion: 'sip-gateway-poc/1',
    protocolVersion: 1,
  });
  if (data.runtimeMode !== 'gateway' || data.protocolVersion !== 1 || !data.sessionCapability) {
    throw new Error('API ไม่ได้คืน gateway runtime protocol v1');
  }
  $('collectionBindingId').value = data.collectionBindingId || '';
  $('sessionCapability').value = data.sessionCapability;
  if (data.gatewayUrl) $('gatewayUrl').value = data.gatewayUrl;
  state.standalone = null;
  log('Session capability received', {
    runtimeMode: data.runtimeMode,
    expiresAtUtc: data.expiresAtUtc,
    leaseExpiresAtUtc: data.leaseExpiresAtUtc,
    collectionId: data.collectionId,
    collectionBindingId: data.collectionBindingId,
  });
  return data;
}

function standaloneSessionInput() {
  const gatewayId = $('standaloneGatewayId').value.trim();
  const host = $('sipHost').value.trim();
  const username = $('sipUsername').value.trim();
  const password = $('sipPassword').value;
  const port = Number($('sipPort').value);
  if (!gatewayId || !host || !username || !password) {
    throw new Error('Standalone mode ต้องมี Gateway ID, SIP host, username และ password');
  }
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('SIP port ไม่ถูกต้อง');
  let collectionId = $('collectionId').value.trim();
  let collectionBindingId = $('collectionBindingId').value.trim();
  if (!collectionId) {
    collectionId = uuid();
    $('collectionId').value = collectionId;
  }
  if (!collectionBindingId) {
    collectionBindingId = uuid();
    $('collectionBindingId').value = collectionBindingId;
  }
  return {
    gatewayId,
    collectionId,
    collectionBindingId,
    sessionId: $('agentSessionId').value.trim(),
    agentInstanceId: $('agentInstanceId').value.trim(),
    sip: {
      host,
      port,
      transport: $('sipTransport').value,
      username,
      password,
      realm: $('sipRealm').value.trim(),
    },
  };
}

async function generateStandaloneSessionCapability() {
  const input = standaloneSessionInput();
  if (!input.sessionId || !input.agentInstanceId) throw new Error('กรุณาสร้าง browser identity ก่อน');
  const existing = state.standalone;
  const userSessionId = existing?.sessionId === input.sessionId ? existing.userSessionId : uuid();
  const now = Math.floor(Date.now() / 1000);
  const claims = {
    v: 1,
    type: 'session',
    iss: 'debt-flow-web-api',
    aud: 'debt-flow-sip-gateway',
    gatewayId: input.gatewayId,
    capabilityId: uuid(),
    iat: now,
    nbf: now - 1,
    exp: now + 50,
    leaseExp: now + 110,
    sessionId: input.sessionId,
    userSessionId,
    collectionId: input.collectionId,
    collectionBindingId: input.collectionBindingId,
    agentInstanceId: input.agentInstanceId,
    protocolVersion: 1,
    sip: input.sip,
  };
  $('sessionCapability').value = await sealCapability(claims);
  state.standalone = {
    gatewayId: input.gatewayId,
    sessionId: input.sessionId,
    userSessionId,
    collectionId: input.collectionId,
    collectionBindingId: input.collectionBindingId,
    sipHost: input.sip.host,
    sipPort: input.sip.port,
  };
  log('Standalone session capability created', {
    gatewayId: input.gatewayId,
    collectionId: input.collectionId,
    collectionBindingId: input.collectionBindingId,
    expiresInSeconds: 50,
  });
}

async function requestCallCapability() {
  const collectionId = $('collectionId').value.trim();
  const bindingId = $('collectionBindingId').value.trim();
  const destination = $('destination').value.trim();
  if (!collectionId || !bindingId || !destination) throw new Error('Collection, binding และ destination ต้องมีค่า');
  const data = await apiPost(`portal/collections/${encodeURIComponent(collectionId)}/phone-agent/call-contexts`, {
    collectionBindingId: bindingId,
    sourceKind: 'manual',
    destination,
    gatewaySessionId: $('agentSessionId').value.trim(),
  });
  if (!data.callId || !data.callContextId || !data.gatewayCapability) {
    throw new Error('API ไม่ได้คืน gateway call capability');
  }
  $('callId').value = data.callId;
  $('callContextId').value = data.callContextId;
  $('callCapability').value = data.gatewayCapability;
  log('Call capability received', { callId: data.callId, callContextId: data.callContextId, expiresAtUtc: data.expiresAtUtc });
}

async function generateStandaloneCallCapability() {
  const binding = state.standalone;
  if (!binding) throw new Error('กรุณาสร้าง Standalone session capability ก่อน');
  const destinationInput = $('destination').value.trim();
  if (!destinationInput) throw new Error('กรุณากรอก destination');
  const destination = canonicalStandaloneDestination(destinationInput, binding);
  // Every standalone attempt represents a new call. Reusing a call ID after a
  // failed SIP/media attempt would collide with its durable outbox event key.
  const callId = uuid();
  const callContextId = uuid();
  $('callId').value = callId;
  $('callContextId').value = callContextId;
  $('activeCallId').value = '';
  const now = Math.floor(Date.now() / 1000);
  $('callCapability').value = await sealCapability({
    v: 1,
    type: 'call',
    iss: 'debt-flow-web-api',
    aud: 'debt-flow-sip-gateway',
    gatewayId: binding.gatewayId,
    capabilityId: uuid(),
    iat: now,
    nbf: now - 1,
    exp: now + 50,
    sessionId: binding.sessionId,
    userSessionId: binding.userSessionId,
    collectionId: binding.collectionId,
    collectionBindingId: binding.collectionBindingId,
    callId,
    callContextId,
    destination,
  });
  log('Standalone call capability created', { callId, callContextId, expiresInSeconds: 50 });
}

function canonicalStandaloneDestination(value, binding) {
  if (/^sips?:/i.test(value)) return value;
  if (value.includes('@')) return `sip:${value}`;
  const host = binding.sipHost.includes(':') && !binding.sipHost.startsWith('[')
    ? `[${binding.sipHost}]`
    : binding.sipHost;
  return `sip:${value}@${host}:${binding.sipPort}`;
}

function send(type, payload) {
  if (!state.socket || state.socket.readyState !== WebSocket.OPEN) throw new Error('WSS ยังไม่เชื่อมต่อ');
  const envelope = {
    protocolVersion: 1,
    kind: 'command',
    messageId: uuid(),
    sentAtUtc: new Date().toISOString(),
    type,
    payload,
  };
  state.socket.send(JSON.stringify(envelope));
  log(`→ ${type}`, payload);
}

function command(type, payload = {}) {
  const commandId = payload.commandId || uuid();
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      state.pendingCommands.delete(commandId);
      reject(new Error(`${type} timeout`));
    }, 15000);
    state.pendingCommands.set(commandId, { type, resolve, reject, timeout });
    try {
      send(type, { ...payload, commandId });
    } catch (error) {
      clearTimeout(timeout);
      state.pendingCommands.delete(commandId);
      reject(error);
    }
  });
}

async function connect() {
  if (state.socket) throw new Error('มี WebSocket อยู่แล้ว กรุณา Disconnect ก่อน');
  const url = $('gatewayUrl').value.trim();
  const collectionId = $('collectionId').value.trim();
  const sessionCapability = $('sessionCapability').value.trim();
  if (!/^wss?:\/\//.test(url) || !collectionId || !sessionCapability) {
    throw new Error('Gateway URL, Collection ID และ session capability ต้องมีค่า');
  }
  await primeCallAudio();
  setPill('wsStatus', 'WSS: connecting');
  const socket = new WebSocket(url);
  state.socket = socket;
  socket.addEventListener('open', () => {
    setPill('wsStatus', 'WSS: connected', 'good');
    try {
      send('session.authenticate', {
        collectionId,
        sessionCapability,
        portalVersion: 'sip-gateway-poc/1',
        supportedProtocolVersions: [1],
      });
      $('sessionCapability').value = '';
    } catch (error) {
      handleFailure(error);
    }
  });
  socket.addEventListener('message', (event) => void run(() => handleMessage(event.data)));
  socket.addEventListener('error', () => log('WebSocket transport error', null, 'error'));
  socket.addEventListener('close', (event) => {
    if (state.socket === socket) state.socket = null;
    setPill('wsStatus', `WSS: closed ${event.code}`, 'bad');
    clearTimeout(state.renewTimer);
    stopCallTone();
    for (const [id, pending] of state.pendingCommands) {
      clearTimeout(pending.timeout);
      pending.reject(new Error(event.reason || 'WSS disconnected'));
      state.pendingCommands.delete(id);
    }
    log('WebSocket closed', { code: event.code, reason: event.reason || 'none' }, event.wasClean ? 'info' : 'error');
  });
}

async function handleMessage(raw) {
  let envelope;
  try {
    envelope = JSON.parse(String(raw));
  } catch {
    log('Invalid JSON from Gateway', null, 'error');
    return;
  }
  log(`← ${envelope.type || 'unknown'}`, envelope.payload || {});
  const payload = envelope.payload && typeof envelope.payload === 'object' ? envelope.payload : {};
  if (envelope.type === 'session.welcome') {
    await setupPeer(payload);
    scheduleRenewal();
  } else if (envelope.type === 'gateway.snapshot') {
    applySnapshot(payload);
  } else if (envelope.type === 'webrtc.answer') {
    await acceptAnswer(payload);
  } else if (envelope.type === 'webrtc.ice_candidate') {
    await acceptRemoteCandidate(payload);
  } else if (envelope.type === 'command_result') {
    settleCommand(payload, null);
  } else if (envelope.type === 'error') {
    settleCommand(payload, new Error(payload.code || 'gateway_command_failed'));
  }
  applyDurableEvent(envelope.type, payload);
}

function settleCommand(payload, error) {
  const commandId = typeof payload.commandId === 'string' ? payload.commandId : '';
  const pending = state.pendingCommands.get(commandId);
  if (!pending) return;
  clearTimeout(pending.timeout);
  state.pendingCommands.delete(commandId);
  if (error || payload.status !== 'completed') pending.reject(error || new Error(`${pending.type} rejected`));
  else pending.resolve(payload);
}

function applySnapshot(payload) {
  if (typeof payload.registration === 'string') {
    state.registrationState = payload.registration;
    setPill('registrationStatus', `SIP: ${payload.registration}`, payload.registration === 'registered' ? 'good' : '');
  }
  if (payload.call && typeof payload.call === 'object' && payload.call.callId) {
    $('activeCallId').value = payload.call.callId;
    $('remoteParty').value = typeof payload.remoteParty === 'string' ? payload.remoteParty : '';
    setCallState(payload.call.state || 'active', payload.call.direction || state.callDirection);
  } else {
    setCallState('idle', '');
  }
  updateCallControls();
}

function applyDurableEvent(type, payload) {
  const data = payload && typeof payload.data === 'object' ? payload.data : payload;
  const sequence = Number(payload.sequence || 0);
  if (Number.isSafeInteger(sequence) && sequence > 0) {
    if (sequence <= state.lastSequence) {
      if ($('autoAck').checked) send('events.ack', { sequence: state.lastSequence });
      return;
    }
    state.lastSequence = Math.max(state.lastSequence, sequence);
    if ($('autoAck').checked) send('events.ack', { sequence: state.lastSequence });
  }
  if (type === 'registration.state_changed' && typeof data.state === 'string') {
    state.registrationState = data.state;
    setPill('registrationStatus', `SIP: ${data.state}`, data.state === 'registered' ? 'good' : '');
    updateCallControls();
  }
  if (!type || !type.startsWith('call.')) return;
  if (typeof data.callId === 'string') $('activeCallId').value = data.callId;
  if (typeof data.remoteParty === 'string') $('remoteParty').value = data.remoteParty;
  if (type === 'call.ended') {
    setCallState('ended', data.direction || state.callDirection, data.endReason || 'unknown');
  } else {
    setCallState(data.state || type, data.direction || state.callDirection);
  }
}

function setCallState(callState, direction = '', endReason = '') {
  state.callState = callState;
  state.callDirection = direction;
  if (callState === 'ended') {
    setPill('callStatus', `Call: ended (${endReason || 'unknown'})`, 'bad');
    stopCallTone();
  } else {
    const suffix = direction ? ` · ${direction}` : '';
    setPill('callStatus', `Call: ${callState}${suffix}`, ['connected', 'ringing', 'incoming'].includes(callState) ? 'good' : '');
    if (callState === 'ringing' || callState === 'incoming') startCallTone(direction === 'inbound' ? 'ringtone' : 'ringback');
    else if (callState === 'connected' || callState === 'answering' || callState === 'idle') stopCallTone();
  }
  updateCallControls();
}

function updateCallControls() {
  const inboundRinging = state.callDirection === 'inbound' && ['incoming', 'ringing'].includes(state.callState);
  const active = !['idle', 'ended'].includes(state.callState);
  $('answer').disabled = !inboundRinging;
  $('reject').disabled = !inboundRinging;
  $('hangup').disabled = !active;
  $('startCall').disabled = active || state.registrationState !== 'registered' || state.peer?.connectionState !== 'connected';
}

async function primeCallAudio() {
  if (!$('enableCallTones').checked) return;
  const AudioContextClass = window.AudioContext || window.webkitAudioContext;
  if (!AudioContextClass) return;
  if (!state.toneContext || state.toneContext.state === 'closed') state.toneContext = new AudioContextClass();
  await state.toneContext.resume().catch(() => {});
}

function startCallTone(kind) {
  if (!$('enableCallTones').checked || state.activeTone === kind) return;
  stopCallTone();
  const generation = state.toneGeneration;
  void primeCallAudio().then(() => {
    const context = state.toneContext;
    if (!context || state.activeTone || generation !== state.toneGeneration || !['incoming', 'ringing'].includes(state.callState)) return;
    state.activeTone = kind;
    const gain = context.createGain();
    gain.gain.value = 0;
    gain.connect(context.destination);
    const frequencies = kind === 'ringtone' ? [440, 480] : [400, 450];
    state.toneOscillators = frequencies.map((frequency) => {
      const oscillator = context.createOscillator();
      oscillator.frequency.value = frequency;
      oscillator.connect(gain);
      oscillator.start();
      return oscillator;
    });
    const onMilliseconds = kind === 'ringtone' ? 1000 : 2000;
    const offMilliseconds = kind === 'ringtone' ? 2000 : 4000;
    const setAudible = (audible) => gain.gain.setTargetAtTime(audible ? 0.025 : 0, context.currentTime, 0.01);
    const cycle = () => {
      setAudible(true);
      state.toneTimers.push(setTimeout(() => setAudible(false), onMilliseconds));
    };
    cycle();
    state.toneTimers.push(setInterval(cycle, onMilliseconds + offMilliseconds));
    state.toneGain = gain;
    log(kind === 'ringtone' ? 'Inbound ringtone started' : 'Outbound ringback started');
  });
}

function stopCallTone() {
  state.toneGeneration++;
  for (const timer of state.toneTimers.splice(0)) clearTimeout(timer);
  for (const oscillator of state.toneOscillators.splice(0)) {
    try { oscillator.stop(); } catch { /* already stopped */ }
    oscillator.disconnect();
  }
  state.toneGain?.disconnect();
  state.toneGain = null;
  state.activeTone = '';
}

async function setupPeer(payload) {
  const turn = payload.turn;
  if (!turn || !Array.isArray(turn.urls) || !turn.username || !turn.password) {
    throw new Error('Gateway welcome ไม่มี TURN credential ที่ถูกต้อง');
  }
  await closeMedia();
  setPill('peerStatus', 'WebRTC: preparing');
  const peer = new RTCPeerConnection({
    iceServers: [{ urls: turn.urls, username: turn.username, credential: turn.password }],
    iceTransportPolicy: $('icePolicy').value,
  });
  state.peer = peer;
  state.pendingCandidates = [];
  state.localCandidates = [];
  state.offerSent = false;
  peer.onicecandidate = (event) => {
    if (!event.candidate) return;
    const candidate = {
      candidate: event.candidate.candidate,
      sdpMid: event.candidate.sdpMid,
      sdpMLineIndex: event.candidate.sdpMLineIndex,
    };
    if (state.offerSent) send('webrtc.ice_candidate', candidate);
    else state.localCandidates.push(candidate);
  };
  peer.ontrack = (event) => void attachRemoteAudio(event.streams[0] || new MediaStream([event.track]));
  peer.onconnectionstatechange = () => {
    const tone = peer.connectionState === 'connected' ? 'good' : peer.connectionState === 'failed' ? 'bad' : '';
    setPill('peerStatus', `WebRTC: ${peer.connectionState}`, tone);
    updateCallControls();
  };
  await replaceMicrophone();
  await negotiatePeer();
}

async function negotiatePeer() {
  const peer = state.peer;
  if (!peer) throw new Error('WebRTC peer ยังไม่ถูกสร้าง');
  if (state.offerSent) return;
  const offer = await peer.createOffer();
  await peer.setLocalDescription(offer);
  send('webrtc.offer', { sdp: offer.sdp || '' });
  setPill('peerStatus', `WebRTC: negotiating${$('useSilentAudio').checked ? ' (silent)' : ''}`);
  state.offerSent = true;
  for (const candidate of state.localCandidates.splice(0)) send('webrtc.ice_candidate', candidate);
  await refreshDevices();
  clearInterval(state.statsTimer);
  state.statsTimer = setInterval(() => void updateICEStats(), 2000);
}

async function createSilentAudioStream() {
  const AudioContextClass = window.AudioContext || window.webkitAudioContext;
  if (!AudioContextClass) throw new Error('Browser นี้ไม่รองรับ silent audio track');
  const context = new AudioContextClass();
  const source = context.createConstantSource();
  const gain = context.createGain();
  const destination = context.createMediaStreamDestination();
  gain.gain.value = 0;
  source.connect(gain);
  gain.connect(destination);
  source.start();
  // On Windows Server/RDP without an audio endpoint, resume() can remain
  // pending indefinitely. Negotiation only needs the live destination track,
  // so do not block SDP offer creation on the audio hardware state.
  void context.resume().catch(() => {});
  const track = destination.stream.getAudioTracks()[0];
  if (!track) {
    source.stop();
    await context.close();
    throw new Error('สร้าง silent audio track ไม่สำเร็จ');
  }
  return { stream: destination.stream, context, source };
}

async function replaceMicrophone() {
  let useSilentAudio = $('useSilentAudio').checked;
  let stream;
  let silent = null;
  if (useSilentAudio) {
    silent = await createSilentAudioStream();
    stream = silent.stream;
  } else {
    const deviceId = $('inputDevice').value;
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        audio: deviceId ? { deviceId: { exact: deviceId } } : true,
        video: false,
      });
    } catch (error) {
      if (error?.name !== 'NotFoundError') {
        setPill('peerStatus', 'WebRTC: microphone unavailable', 'bad');
        throw new Error(`${error?.name || 'MicrophoneError'}: เปิด Silent audio เพื่อทดสอบโดยไม่มีไมค์`);
      }
      useSilentAudio = true;
      $('useSilentAudio').checked = true;
      $('inputDevice').disabled = true;
      $('replaceMic').textContent = 'Apply silent audio';
      silent = await createSilentAudioStream();
      stream = silent.stream;
      log('No microphone detected; switched to Silent audio automatically', null, 'warn');
    }
  }
  const track = stream.getAudioTracks()[0];
  if (!track) throw new Error('ไม่พบ microphone track');
  track.enabled = !$('mute').checked;
  const sender = state.peer?.getSenders().find((item) => item.track?.kind === 'audio');
  if (sender) await sender.replaceTrack(track);
  else state.peer?.addTrack(track, stream);
  state.localStream?.getTracks().forEach((item) => item.stop());
  state.silentAudioSource?.stop();
  await state.silentAudioContext?.close().catch(() => {});
  state.localStream = stream;
  state.silentAudioContext = silent?.context || null;
  state.silentAudioSource = silent?.source || null;
  log(useSilentAudio ? 'Silent audio track enabled' : 'Microphone track enabled');
}

async function attachRemoteAudio(stream) {
  state.remoteStream = stream;
  const audio = $('remoteAudio');
  audio.srcObject = stream;
  if ('setSinkId' in audio && $('outputDevice').value) await audio.setSinkId($('outputDevice').value);
  await audio.play().catch(() => log('Browser blocked autoplay; กด Play ที่ audio control', null, 'warn'));
}

async function acceptAnswer(payload) {
  if (!state.peer || typeof payload.sdp !== 'string') throw new Error('WebRTC answer ไม่ถูกต้อง');
  await state.peer.setRemoteDescription({ type: 'answer', sdp: payload.sdp });
  for (const candidate of state.pendingCandidates.splice(0)) await state.peer.addIceCandidate(candidate);
  setPill('peerStatus', `WebRTC: answer ready${$('useSilentAudio').checked ? ' (silent)' : ''}`, 'good');
  log('WebRTC media negotiation completed');
}

async function acceptRemoteCandidate(payload) {
  if (!state.peer || typeof payload.candidate !== 'string') return;
  const candidate = { candidate: payload.candidate, sdpMid: payload.sdpMid ?? null, sdpMLineIndex: payload.sdpMLineIndex ?? null };
  if (!state.peer.remoteDescription) state.pendingCandidates.push(candidate);
  else await state.peer.addIceCandidate(candidate);
}

async function refreshDevices() {
  const devices = await navigator.mediaDevices.enumerateDevices();
  populateDevices('inputDevice', devices.filter((item) => item.kind === 'audioinput'), 'Default microphone');
  populateDevices('outputDevice', devices.filter((item) => item.kind === 'audiooutput'), 'Default speaker');
}

function populateDevices(id, devices, fallback) {
  const select = $(id);
  const selected = select.value;
  select.replaceChildren(new Option(fallback, ''));
  devices.forEach((device, index) => select.add(new Option(device.label || `${device.kind} ${index + 1}`, device.deviceId)));
  if ([...select.options].some((option) => option.value === selected)) select.value = selected;
}

async function updateICEStats() {
  if (!state.peer) return;
  const stats = await state.peer.getStats();
  let selected;
  stats.forEach((item) => {
    if (item.type === 'candidate-pair' && item.state === 'succeeded' && (item.selected || item.nominated)) selected = item;
  });
  if (!selected) return;
  const local = stats.get(selected.localCandidateId);
  const remote = stats.get(selected.remoteCandidateId);
  const rtt = Number.isFinite(selected.currentRoundTripTime) ? `${Math.round(selected.currentRoundTripTime * 1000)} ms` : '—';
  $('iceRoute').textContent = `Selected ICE route: ${local?.candidateType || '?'} → ${remote?.candidateType || '?'} · RTT ${rtt}`;
}

async function closeMedia() {
  clearInterval(state.statsTimer);
  state.statsTimer = null;
  stopCallTone();
  state.localStream?.getTracks().forEach((track) => track.stop());
  state.remoteStream?.getTracks().forEach((track) => track.stop());
  state.silentAudioSource?.stop();
  await state.silentAudioContext?.close().catch(() => {});
  state.localStream = null;
  state.remoteStream = null;
  state.silentAudioSource = null;
  state.silentAudioContext = null;
  if (state.peer) state.peer.close();
  state.peer = null;
  $('remoteAudio').srcObject = null;
  $('iceRoute').textContent = 'Selected ICE route: —';
  setPill('peerStatus', 'WebRTC: idle');
}

async function disconnect() {
  clearTimeout(state.renewTimer);
  if (state.socket?.readyState === WebSocket.OPEN) {
    if (!['idle', 'ended'].includes(state.callState)) {
      await command('call.hangup', { callId: activeCallId() }).catch((error) => log(`Hangup before disconnect failed: ${error.message}`, null, 'warn'));
    }
    if (!['idle', 'unregistered', 'unknown'].includes(state.registrationState)) {
      await command('registration.stop').catch((error) => log(`UNREGISTER before disconnect failed: ${error.message}`, null, 'warn'));
    }
  }
  if (state.socket && state.socket.readyState < WebSocket.CLOSING) state.socket.close(1000, 'poc_disconnect');
  state.socket = null;
  await closeMedia();
  state.registrationState = 'unknown';
  setPill('registrationStatus', 'SIP: unknown');
  setCallState('idle', '');
}

async function renewSession() {
  if ($('apiBase').value.trim()) await requestSessionCapability();
  else if (state.standalone) await generateStandaloneSessionCapability();
  const capability = $('sessionCapability').value.trim();
  if (!capability) throw new Error('ไม่มี session capability สำหรับ renew');
  await command('session.renew', { sessionCapability: capability });
  $('sessionCapability').value = '';
  log('Session lease renewed');
  scheduleRenewal();
}

function scheduleRenewal() {
  clearTimeout(state.renewTimer);
  if (!$('autoRenew').checked || (!$('apiBase').value.trim() && !state.standalone)) return;
  state.renewTimer = setTimeout(() => void run(renewSession), 45000);
}

async function startCall() {
  await waitForWebRTCMedia();
  $('remoteParty').value = '';
  if (state.standalone && (!$('callContextId').value.trim() || !$('callCapability').value.trim())) {
    await generateStandaloneCallCapability();
  }
  const callId = $('callId').value.trim();
  const callContextId = $('callContextId').value.trim();
  const gatewayCapability = $('callCapability').value.trim();
  if (!callId || !callContextId || !gatewayCapability) throw new Error('Call ID, context และ capability ต้องมีค่า');
  $('activeCallId').value = callId;
  const result = command('call.start', { callId, callContextId, gatewayCapability });
  $('callCapability').value = '';
  await result;
}

async function waitForWebRTCMedia(timeoutMilliseconds = 30000) {
  if (!state.peer) throw new Error('WebRTC ยังไม่เริ่ม กรุณา Connect ก่อนโทร');
  const deadline = Date.now() + timeoutMilliseconds;
  while (!state.peer?.remoteDescription || state.peer.connectionState !== 'connected') {
    if (!state.peer || ['failed', 'closed'].includes(state.peer.connectionState)) {
      throw new Error(`WebRTC ${state.peer?.connectionState || 'closed'} กรุณา Disconnect แล้ว Connect ใหม่`);
    }
    if (Date.now() >= deadline) {
      throw new Error(`WebRTC media ยังไม่พร้อมภายใน ${Math.round(timeoutMilliseconds / 1000)} วินาที (state: ${state.peer?.connectionState || 'closed'})`);
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}

function activeCallId() {
  const value = $('activeCallId').value.trim() || $('callId').value.trim();
  if (!value) throw new Error('ไม่มี active call ID');
  return value;
}

async function run(action) {
  try {
    await action();
  } catch (error) {
    handleFailure(error);
  }
}

function handleFailure(error) {
  const message = error instanceof Error ? error.message : String(error);
  log(message, null, 'error');
}

function bind() {
  $('newIdentity').addEventListener('click', () => newIdentity());
  $('requestSession').addEventListener('click', () => void run(requestSessionCapability));
  $('generateSession').addEventListener('click', () => void run(generateStandaloneSessionCapability));
  $('requestCall').addEventListener('click', () => void run(requestCallCapability));
  $('generateCall').addEventListener('click', () => void run(generateStandaloneCallCapability));
  $('connect').addEventListener('click', () => void run(connect));
  $('renew').addEventListener('click', () => void run(renewSession));
  $('disconnect').addEventListener('click', () => void run(disconnect));
  $('register').addEventListener('click', () => void run(() => command('registration.start')));
  $('unregister').addEventListener('click', () => void run(() => command('registration.stop')));
  $('getState').addEventListener('click', () => void run(async () => send('state.get', { commandId: uuid() })));
  $('startCall').addEventListener('click', () => void run(startCall));
  $('answer').addEventListener('click', () => void run(() => command('call.answer', { callId: activeCallId() })));
  $('reject').addEventListener('click', () => void run(() => command('call.reject', { callId: activeCallId() })));
  $('hangup').addEventListener('click', () => void run(() => command('call.hangup', { callId: activeCallId() })));
  $('refreshDevices').addEventListener('click', () => void run(refreshDevices));
  $('replaceMic').addEventListener('click', () => void run(replaceMicrophone));
  $('useSilentAudio').addEventListener('change', () => {
    $('inputDevice').disabled = $('useSilentAudio').checked;
    $('replaceMic').textContent = $('useSilentAudio').checked ? 'Apply silent audio' : 'Switch microphone';
    if (state.peer) void run(async () => {
      await replaceMicrophone();
      await negotiatePeer();
    });
  });
  $('enableCallTones').addEventListener('change', () => {
    if (!$('enableCallTones').checked) stopCallTone();
    else if (state.callState === 'ringing' || state.callState === 'incoming') startCallTone(state.callDirection === 'inbound' ? 'ringtone' : 'ringback');
  });
  $('mute').addEventListener('change', () => {
    state.localStream?.getAudioTracks().forEach((track) => { track.enabled = !$('mute').checked; });
  });
  $('outputDevice').addEventListener('change', () => void run(async () => {
    if ('setSinkId' in $('remoteAudio')) await $('remoteAudio').setSinkId($('outputDevice').value);
  }));
  $('clearLog').addEventListener('click', () => { state.logEntries = []; $('log').textContent = ''; });
  for (const digit of '123A456B789C*0#D') {
    const button = document.createElement('button');
    button.type = 'button';
    button.textContent = digit;
    button.addEventListener('click', () => void run(() => command('call.dtmf', { callId: activeCallId(), digit })));
    $('dtmfPad').append(button);
  }
  window.addEventListener('beforeunload', () => {
    if (state.socket?.readyState === WebSocket.OPEN && !['idle', 'unregistered', 'unknown'].includes(state.registrationState)) {
      try { send('registration.stop', { commandId: uuid() }); } catch { /* page is closing */ }
    }
    state.socket?.close(1000, 'poc_unload');
    stopCallTone();
    state.localStream?.getTracks().forEach((track) => track.stop());
  });
}

bind();
newIdentity();
updateCallControls();
void run(refreshDevices);
