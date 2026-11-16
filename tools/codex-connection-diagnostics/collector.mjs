import fs from 'node:fs';
import path from 'node:path';
import http from 'node:http';
import net from 'node:net';
import crypto from 'node:crypto';
import { pathToFileURL } from 'node:url';
import { DatabaseSync } from 'node:sqlite';

const kinds = new Set(['request_start', 'request_context_done', 'tls_handshake', 'http_bind', 'dial_error', 'wire_open', 'wire_progress', 'wire_read_error', 'wire_write_error', 'wire_close_start', 'wire_close_done', 'ws_bind', 'ws_send_start', 'ws_send_done', 'ws_send_error', 'ws_read_error', 'ws_terminal', 'ws_delivered', 'ws_body_close', 'ws_ping_received', 'ws_pong_received']);
const errors = new Set(['', 'os_error', 'cancelled', 'deadline', 'closed', 'unexpected_eof', 'eof', 'timeout', 'websocket_close', 'tls_record', 'other']);
const numeric = ['at_ns', 'pid', 'boot', 'route', 'request', 'connection', 'created_ns', 'trace', 'read_bytes', 'write_bytes', 'last_read_ns', 'last_write_ns', 'close_started_ns', 'close_finished_ns', 'errno', 'close_status', 'frames', 'frame_bytes', 'last_frame_ns', 'delivered_bytes', 'last_delivered_ns', 'dropped', 'pings', 'pongs', 'last_ping_ns', 'last_pong_ns', 'read_error_ns', 'write_error_ns', 'read_errno', 'write_errno'];
const endpoint = value => typeof value === 'string' && /^(?:\[[0-9a-fA-F:]+\]|[0-9.]+):\d{1,5}$/.test(value) ? value : undefined;
export const hash = value => crypto.createHash('sha256').update(String(value)).digest('hex').slice(0, 16);

// Accept only the versioned metadata schema, even if a future logger adds
// headers, error messages or response content to the same object.
export function safeEvent(input) {
  if (!input || !kinds.has(input.event)) return null;
  const out = { event: input.event };
  for (const key of numeric) {
    if (Number.isFinite(input[key])) out[key] = input[key];
    else if ((key.endsWith('_ns') || key === 'boot') && /^\d{1,20}$/.test(input[key] || '')) out[key] = input[key];
  }
  for (const key of ['reused', 'cancelled', 'terminal']) if (typeof input[key] === 'boolean') out[key] = input[key];
  for (const key of ['local', 'peer', 'downstream']) if (endpoint(input[key])) out[key] = input[key];
  if (/^[0-9a-f]{16}$/.test(input.session || '')) out.session = input.session;
  if (errors.has(input.error)) out.error = input.error;
  for (const key of ['read_error', 'write_error']) if (errors.has(input[key])) out[key] = input[key];
  return out;
}

export function errorClass(text) {
  text = String(text || '');
  for (const [pattern, code] of [[/idle timeout waiting for websocket/i, 'websocket_idle_timeout'], [/no recent network activity/i, 'quic_idle_timeout'], [/unexpected EOF/i, 'unexpected_eof'], [/reset by peer|ECONNRESET/i, 'reset'], [/use of closed network connection/i, 'closed'], [/context cancel/i, 'cancelled'], [/deadline|timed? out|timeout/i, 'timeout'], [/EOF/i, 'eof']]) if (pattern.test(text)) return code;
  return 'other';
}

function sourceOf(c) {
  const host = String(c.metadata?.sourceIP || '').replace(/^::ffff:/, '');
  return net.isIP(host) ? `${host.includes(':') ? `[${host}]` : host}:${c.metadata.sourcePort}` : null;
}

// A source port alone is insufficient after reuse. Require the exact source,
// proxy listener port, and compatible birth times as well as a stable wire ID.
export function matchesConnection(wire, clash) {
  const proxyPort = Number(wire.peer?.slice(wire.peer.lastIndexOf(':') + 1));
  const inbound = clash.metadata?.inboundPort;
  return wire.local === sourceOf(clash) && (!inbound || Number(inbound) === proxyPort) &&
    Math.abs(Number(wire.created_ns) / 1e6 - Date.parse(clash.start)) < 5000;
}

async function main() {
  const args = new Map();
  for (let i = 2; i < process.argv.length; i += 2) args.set(process.argv[i], process.argv[i + 1]);
  const required = key => {
    const value = args.get(key);
    if (!value || !path.isAbsolute(value)) throw Error(`Expected an absolute path for ${key}`);
    return value;
  };
  const root = required('--root'), appLog = required('--app-log'), codexHome = required('--codex-home');
  const { default: WebSocket } = await import(pathToFileURL(required('--ws-module')).href);
  const logs = path.join(root, 'logs'), statusFile = path.join(root, 'status.json');
  fs.mkdirSync(logs, { recursive: true });
  try {
    const previous = JSON.parse(fs.readFileSync(statusFile, 'utf8'));
    if (Date.now() - Date.parse(previous.updatedAt) < 15000) {
      process.kill(previous.pid, 0);
      return;
    }
  } catch {}
  fs.rmSync(path.join(root, 'STOP'), { force: true });
  let offset = Math.max(0, (fs.existsSync(appLog) ? fs.statSync(appLog).size : 0) - 1024 * 1024), pending = '';
  let pipe = null, ws = null, db = null, lastRow = 0, lastProgress = 0, lastSweep = 0, lastV2At = null;
  let events = 0, bindings = 0;
  const wires = new Map(), connections = new Map(), joins = new Set();
  const day = () => new Intl.DateTimeFormat('sv-SE', { timeZone: 'Asia/Shanghai' }).format(new Date());
  const emit = (kind, fields = {}) => {
    fs.appendFileSync(path.join(logs, `${day()}.jsonl`), JSON.stringify({ observedAt: new Date().toISOString(), kind, ...fields }) + '\n');
    events++;
  };
  function tail() {
    if (!fs.existsSync(appLog)) return;
    const size = fs.statSync(appLog).size;
    if (size < offset) { offset = 0; pending = ''; }
    if (size === offset) return;
    if (size - offset > 8 * 1024 * 1024) { emit('tail_gap'); offset = size; pending = ''; return; }
    const fd = fs.openSync(appLog, 'r'), buffer = Buffer.alloc(size - offset);
    try { fs.readSync(fd, buffer, 0, buffer.length, offset); } finally { fs.closeSync(fd); }
    offset = size;
    const lines = (pending + buffer.toString('utf8')).split('\n');
    pending = lines.pop();
    for (const line of lines) {
      const at = line.indexOf('codex-transport-v2: ');
      if (at < 0) continue;
      let e;
      try { e = safeEvent(JSON.parse(line.slice(at + 20))); } catch { continue; }
      if (!e) continue;
      lastV2At = new Date().toISOString();
      emit('magpie_transport', e);
      if (e.connection && e.created_ns && e.local && e.peer) {
        const key = `${e.pid}:${e.boot}:${e.connection}`;
        const previous = wires.get(key);
        wires.set(key, { ...previous, ...e, seenAt: Date.now() });
      }
    }
  }
  function request(endpoint) {
    return new Promise((resolve, reject) => {
      const q = http.get({ socketPath: pipe, path: endpoint }, res => {
        const buffers = []; let bytes = 0;
        res.on('data', b => { bytes += b.length; if (bytes > 8 * 1024 * 1024) q.destroy(Error('limit')); else buffers.push(b); });
        res.on('end', () => { try { if (res.statusCode !== 200) throw Error('status'); resolve(JSON.parse(Buffer.concat(buffers).toString('utf8'))); } catch (e) { reject(e); } });
        res.on('error', reject);
      });
      const timeout = setTimeout(() => q.destroy(Error('timeout')), 2000);
      q.on('close', () => clearTimeout(timeout));
      q.on('error', reject);
    });
  }
  async function proxy() {
    if (!pipe) {
      const name = fs.readdirSync('\\\\.\\pipe\\').find(name => name.startsWith('verge-mihomo-production-'));
      if (!name) return;
      pipe = `\\\\.\\pipe\\${name}`;
      const version = await request('/version');
      emit('clash_attached', { version: String(version.version).slice(0, 40) });
    }
    if (!ws) {
      ws = new WebSocket('ws://localhost/logs?level=warning', { createConnection: () => net.connect(pipe), handshakeTimeout: 2000, maxPayload: 1024 * 1024 });
      const current = ws;
      ws.on('error', () => {});
      ws.on('close', () => { if (ws === current) ws = null; });
      ws.on('message', b => {
        try {
          const text = String(JSON.parse(b.toString('utf8')).payload || '');
          const sourcePorts = [...text.matchAll(/127\.0\.0\.1:(\d+)/g)].map(m => Number(m[1]));
          if (!text.includes('chatgpt.com') && ![...wires.values()].some(w => sourcePorts.includes(Number(w.local?.slice(w.local.lastIndexOf(':') + 1))))) return;
          emit('clash_warning', { error: errorClass(text), sourcePorts, quic: /quic|hysteria/i.test(text), tls: /tls|certificate/i.test(text), dial: /dial|connect/i.test(text) });
        } catch {}
      });
    }
    const data = await request('/connections'), next = new Map();
    const progress = Date.now() - lastProgress > 10000;
    for (const c of data.connections || []) {
      const matches = [...wires.entries()].filter(([, w]) => matchesConnection(w, c));
      if (c.metadata?.host !== 'chatgpt.com' && !matches.length) continue;
      const id = hash(c.id);
      const safe = { id, source: sourceOf(c), start: c.start, upload: c.upload, download: c.download, chains: (c.chains || []).map(s => String(s).slice(0, 120)), network: c.metadata?.network };
      next.set(id, safe);
      if (!connections.has(id)) emit('clash_open', safe);
      else if (progress) emit('clash_progress', safe);
      for (const [key, w] of matches) {
        const join = `${key}:${id}`;
        if (!joins.has(join)) {
          joins.add(join); bindings++;
          emit('connection_join', { pid: w.pid, boot: w.boot, connection: w.connection, clash: id, local: w.local, peer: w.peer, created_ns: w.created_ns, clashStart: c.start, chains: safe.chains });
        }
      }
    }
    for (const [id, c] of connections) if (!next.has(id)) emit('clash_closed', c);
    connections.clear();
    for (const [id, c] of next) connections.set(id, c);
    if (progress) lastProgress = Date.now();
    for (const [key, w] of wires) if (Date.now() - w.seenAt > 60 * 60 * 1000) wires.delete(key);
    if (joins.size > 16384) joins.clear();
  }
  function retries() {
    if (!db) {
      db = new DatabaseSync(path.join(codexHome, 'logs_2.sqlite'), { readOnly: true });
      db.exec('PRAGMA busy_timeout=500');
      lastRow = Number(db.prepare('SELECT max(id) AS id FROM logs').get().id || 0);
      return;
    }
    const max = Number(db.prepare('SELECT max(id) AS id FROM logs').get().id || 0);
    if (max < lastRow) lastRow = 0;
    for (const r of db.prepare("SELECT id,ts,thread_id,feedback_log_body FROM logs WHERE id>? AND id<=? AND target='codex_core::responses_retry' ORDER BY id").all(lastRow, max)) {
      emit('codex_retry', { eventAt: new Date(Number(r.ts) * 1000).toISOString(), session: hash(r.thread_id), error: errorClass(r.feedback_log_body), retry: Number(/retries=(\d+)/.exec(r.feedback_log_body || '')?.[1] || 0) });
    }
    lastRow = max;
  }
  function sweep() {
    const today = `${day()}.jsonl`;
    const files = fs.readdirSync(logs).filter(n => /^\d{4}-\d\d-\d\d\.jsonl$/.test(n)).map(name => ({ name, ...fs.statSync(path.join(logs, name)) })).sort((a, b) => a.name.localeCompare(b.name));
    let total = files.reduce((sum, f) => sum + f.size, 0);
    for (const f of files) if (f.name !== today && (Date.now() - f.mtimeMs > 14 * 86400000 || total > 128 * 1024 * 1024)) { fs.unlinkSync(path.join(logs, f.name)); total -= f.size; }
    const current = path.join(logs, today);
    if (fs.existsSync(current) && fs.statSync(current).size > 128 * 1024 * 1024) { fs.writeFileSync(current, ''); emit('size_rollover'); }
  }
  emit('collector_started', { pid: process.pid, schema: 2 });
  while (!fs.existsSync(path.join(root, 'STOP'))) {
    try { tail(); } catch { emit('app_log_read_error'); }
    try { retries(); } catch { emit('codex_log_read_error'); try { db?.close(); } catch {} db = null; }
    try { await proxy(); } catch { emit('clash_read_error'); pipe = null; ws?.close(); ws = null; }
    fs.writeFileSync(statusFile, JSON.stringify({ pid: process.pid, running: true, updatedAt: new Date().toISOString(), events, bindings, lastV2At, activeClashConnections: connections.size, observedWires: wires.size, proxyConnected: !!pipe }));
    if (Date.now() - lastSweep > 60000) { sweep(); lastSweep = Date.now(); }
    await new Promise(resolve => setTimeout(resolve, 500));
  }
  ws?.close(); db?.close();
  fs.writeFileSync(statusFile, JSON.stringify({ pid: process.pid, running: false, updatedAt: new Date().toISOString(), events, bindings, lastV2At }));
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  main().catch(() => { process.stderr.write('Connection diagnostics failed; no credentials or raw errors were printed.\n'); process.exitCode = 1; });
}
