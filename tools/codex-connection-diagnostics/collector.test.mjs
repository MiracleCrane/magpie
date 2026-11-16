import test from 'node:test';
import assert from 'node:assert/strict';
import { safeEvent, matchesConnection, errorClass } from './collector.mjs';

test('metadata schema preserves exact times and rejects private additions', () => {
  const at = '1794813517005678900';
  const e = safeEvent({ event: 'ws_read_error', at_ns: at, boot: at, connection: 3, route: 12,
    session: '0123456789abcdef', local: '127.0.0.1:12345', peer: '127.0.0.1:7897',
    error: 'eof', read_error: 'eof', read_error_ns: at, reason: 'PRIVATE', headers: { Authorization: 'PRIVATE' }, body: 'PRIVATE' });
  assert.equal(e.at_ns, at);
  assert.equal(e.read_error_ns, at);
  assert.equal(e.connection, 3);
  assert.equal(e.error, 'eof');
  assert.ok(!JSON.stringify(e).includes('PRIVATE'));
  assert.equal(safeEvent({ event: 'arbitrary PRIVATE' }), null);
  assert.ok(!safeEvent({ event: 'wire_open', local: 'PRIVATE', session: 'PRIVATE' }).local);
});

test('proxy join rejects a reused port and a different proxy listener', () => {
  const start = '2026-11-16T07:35:18.000Z';
  const wire = { local: '127.0.0.1:12345', peer: '127.0.0.1:7897', created_ns: String(BigInt(Date.parse(start)) * 1000000n) };
  const clash = { start, metadata: { sourceIP: '127.0.0.1', sourcePort: 12345, inboundPort: 7897 } };
  assert.equal(matchesConnection(wire, clash), true);
  assert.equal(matchesConnection(wire, { ...clash, start: '2026-11-16T07:45:18.000Z' }), false);
  assert.equal(matchesConnection(wire, { ...clash, metadata: { ...clash.metadata, inboundPort: 7898 } }), false);
  assert.equal(matchesConnection(wire, { ...clash, metadata: { ...clash.metadata, sourcePort: 12346 } }), false);
  const endedWire = { ...wire, close_finished_ns: String(BigInt(wire.created_ns) + 100000000n) };
  assert.equal(matchesConnection(endedWire, { ...clash, start: '2026-11-16T07:35:18.101Z' }), false);
  assert.equal(matchesConnection(wire, { ...clash, metadata: { ...clash.metadata, inboundPort: undefined } }), false);
});

test('errors are classified without copying diagnostic text', () => {
  assert.equal(errorClass('stream disconnected: idle timeout waiting for websocket PRIVATE'), 'websocket_idle_timeout');
  assert.equal(errorClass('QUIC: no recent network activity PRIVATE'), 'quic_idle_timeout');
  assert.equal(errorClass('PRIVATE'), 'other');
});
