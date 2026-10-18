import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
import http from 'node:http'
import { createHash } from 'node:crypto'

const modules = new Map()
async function load(name) {
  if (modules.has(name)) return modules.get(name)
  let code = ts.transpileModule(await readFile(new URL(`../src/lib/${name}.ts`, import.meta.url), 'utf8'), {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
  }).outputText
  for (const dep of ['protocol', 'ulid', 'api']) {
    if (code.includes(`from './${dep}'`)) code = code.replaceAll(`from './${dep}'`, `from '${await load(dep)}'`)
  }
  const url = `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
  modules.set(name, url)
  return url
}
const { WSClient } = await import(await load('ws'))
const apiModule = await import(await load('api'))
const original = { setTimeout, clearTimeout, fetch, WebSocket: globalThis.WebSocket, location: globalThis.location }
let now = 0, timerID = 0, timers = new Map(), sockets = [], requests = [], responses = []
const flush = async () => { for (let i = 0; i < 30; i++) await Promise.resolve() }
globalThis.setTimeout = (fn, delay) => { const id = ++timerID; timers.set(id, { fn, at: now + delay }); return id }
globalThis.clearTimeout = id => timers.delete(id)
async function advance(ms) {
  const end = now + ms
  for (;;) {
    const next = [...timers].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0]
    if (!next) break
    now = next[1].at
    timers.delete(next[0]); next[1].fn(); await flush()
  }
  now = end; await flush()
}
class Socket {
  static OPEN = 1
  readyState = 0
  sent = []
  constructor() { sockets.push(this) }
  open() { this.readyState = 1; this.onopen?.({}) }
  close(code = 1000) { this.readyState = 3; this.onclose?.({ code, reason: '' }) }
  send(raw) { this.sent.push(JSON.parse(raw)) }
  pong() {
    const ping = this.sent.findLast(e => e.type === 'ping')
    assert.ok(ping, 'client sends a liveness probe')
    this.onmessage?.({ data: JSON.stringify({ v: 1, type: 'pong', ts: 1, reply_to: ping.request_id }) })
  }
}
globalThis.WebSocket = Socket
globalThis.location = { protocol: 'https:', host: 'test.local' }
const ticket = () => Response.json({ ticket: 'one-use', protocol: { min: 1, max: 1 } })
globalThis.fetch = async (url, opts) => {
  requests.push({ url, opts })
  const next = responses.shift()
  if (next instanceof Error) throw next
  return typeof next === 'function' ? next() : next ?? ticket()
}
const clients = []
function setup() {
  for (const client of clients) client.dispose()
  timers = new Map(); sockets = []; requests = []; responses = []; now = 0
  const fatal = [], ready = []
  const client = new WSClient({ onFatal: e => fatal.push(e), onReady: r => ready.push(r) })
  clients.push(client)
  return { client, fatal, ready }
}
try {
  {
    const { client, fatal, ready } = setup()
    client.connect(); await flush(); sockets[0].open(); sockets[0].close(1006)
    responses.push(new TypeError('offline'))
    await advance(1500)
    assert.equal(client.getState(), 'reconnecting', 'transient ticket failure must keep retrying')
    assert.deepEqual(fatal, [])
    await advance(3000); sockets.at(-1).open()
    assert.deepEqual(ready, [false, true], 'recovery asks the page to reattach existing session')
    const count = sockets.length
    client.connect(); await advance(1500)
    assert.equal(sockets.length, count, 'connect on open socket is idempotent')
  }
  {
    const { client } = setup()
    responses.push(() => new Promise(() => {}))
    client.connect(); client.connect(); await flush()
    assert.equal(requests.length, 1, 'only one ticket request in flight')
    await advance(32_000)
    assert.ok(sockets.length > 0, 'a hung ticket request cannot block future attempts')
    sockets.at(-1).open()
    assert.equal(client.getState(), 'open')
  }
  {
    const { client } = setup()
    client.connect(); await flush()
    const stale = sockets[0]
    await advance(32_000)
    assert.ok(sockets.length > 1, 'handshake without open/close times out')
    sockets.at(-1).open(); stale.onclose?.({ code: 1006, reason: '' }); stale.onopen?.({})
    assert.equal(client.isOpen(), true, 'old socket callbacks cannot overwrite recovered connection')
  }
  {
    const { client } = setup()
    client.connect(); await flush(); sockets[0].open()
    await advance(20_000); sockets[0].pong(); await flush()
    assert.equal(client.isOpen(), true)
    await advance(32_000)
    assert.equal(sockets[0].readyState, 3, 'a silent half-open socket is retired')
    assert.ok(sockets.length > 1, 'half-open connection reconnects without a refresh')
  }
  {
    const { client, fatal } = setup()
    responses.push(new Response('temporarily unavailable', { status: 503 }))
    client.connect(); await flush()
    assert.equal(client.getState(), 'reconnecting', 'proxy HTML error is recoverable too')
    await advance(1500); sockets[0].open()
    assert.deepEqual(fatal, [])
  }
  {
    const { client } = setup()
    client.connect(); await flush(); sockets[0].open(); sockets[0].close(1006)
    client.connect(); client.connect(); await flush(); sockets[1].open()
    await advance(1500)
    assert.equal(sockets.length, 2, 'manual retry cancels queued retry and does not race it')
  }
  {
    const { client, fatal } = setup()
    client.connect(); await flush(); sockets[0].open()
    await advance(20_000)
    assert.equal(sockets[0].sent.at(-1).type, 'ping')
    sockets[0].close(4401); await flush(); await advance(120_000)
    assert.equal(client.getState(), 'closed')
    assert.equal(fatal.length, 1)
    assert.equal(sockets.length, 1, 'rejected heartbeat must not revive a fatal connection')
  }
  {
    const { client, fatal } = setup()
    responses.push(Response.json({}, { status: 401 }), Response.json({}, { status: 401 }))
    client.connect(); await flush(); await advance(120_000)
    assert.equal(client.getState(), 'closed'); assert.equal(fatal.length, 1)
    assert.equal(requests.length, 2, 'invalid credentials do not create an endless retry loop')
  }
  {
    const { client, fatal } = setup()
    let resolve
    responses.push(() => new Promise(r => { resolve = r }))
    client.connect(); await flush(); client.dispose(); resolve(ticket()); await flush(); await advance(120_000)
    assert.equal(sockets.length, 0); assert.deepEqual(fatal, [])
  }
  {
    setup()
    let lost = 0
    apiModule.setAccessToken('existing-access')
    apiModule.setAuthLostHandler(() => lost++)
    responses.push(Response.json({}, { status: 401 }), new TypeError('offline during refresh'))
    await assert.rejects(apiModule.api.wsTicket(), e => e.retryable && e.status !== 401)
    assert.equal(lost, 0, 'transient refresh failure must not log the user out')
    assert.equal(apiModule.getAccessToken(), 'existing-access')
    responses.push(Response.json({}, { status: 401 }), new Response('unavailable', { status: 503 }))
    await assert.rejects(apiModule.api.wsTicket(), e => e.retryable && e.status === 503)
    assert.equal(lost, 0)
    responses.push(Response.json({}, { status: 401 }), Response.json({}, { status: 401 }))
    await assert.rejects(apiModule.api.wsTicket(), e => e.status === 401)
    assert.equal(lost, 1, 'confirmed invalid refresh credentials still log out')
  }
  console.log('PASS reconnect: ticket outage, stalled handshake, stale callbacks, half-open socket, auth failure and dispose')
} finally {
  for (const client of clients) client.dispose()
  Object.assign(globalThis, original)
}

// Exercise the browser client's actual fetch/WebSocket implementations against
// a local HTTP upgrade server, including one failed ticket during reconnection.
let unavailable = false, outages = 0
const peers = new Set()
const server = http.createServer((req, res) => {
  if (unavailable) {
    unavailable = false; outages++
    res.writeHead(503); res.end('proxy unavailable'); return
  }
  res.writeHead(200, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify({ ticket: 'local-ticket', protocol: { min: 1, max: 1 } }))
})
server.on('upgrade', (req, socket) => {
  const accept = createHash('sha1').update(req.headers['sec-websocket-key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64')
  socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`)
  peers.add(socket); socket.on('close', () => peers.delete(socket)); socket.on('error', () => {})
  let buffer = Buffer.alloc(0)
  socket.on('data', chunk => {
    buffer = Buffer.concat([buffer, chunk])
    while (buffer.length >= 2) {
      const opcode = buffer[0] & 15
      let length = buffer[1] & 127, offset = 2
      if (length === 126) { if (buffer.length < 4) return; length = buffer.readUInt16BE(2); offset = 4 }
      assert.notEqual(length, 127, 'fixture only accepts small messages')
      const masked = (buffer[1] & 128) !== 0
      if (buffer.length < offset + (masked ? 4 : 0) + length) return
      const mask = masked ? buffer.subarray(offset, offset + 4) : null
      if (masked) offset += 4
      const payload = Buffer.from(buffer.subarray(offset, offset + length))
      buffer = buffer.subarray(offset + length)
      if (mask) for (let i = 0; i < length; i++) payload[i] ^= mask[i % 4]
      if (opcode === 8) { socket.end(); continue }
      if (opcode !== 1) continue
      const env = JSON.parse(payload.toString())
      if (env.type !== 'ping') continue
      const body = Buffer.from(JSON.stringify({ v: 1, type: 'pong', ts: Date.now(), reply_to: env.request_id }))
      assert.ok(body.length < 126)
      socket.write(Buffer.concat([Buffer.from([0x81, body.length]), body]))
    }
  })
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const host = `127.0.0.1:${server.address().port}`
globalThis.location = { protocol: 'http:', host }
globalThis.fetch = (url, opts) => original.fetch(new URL(url, `http://${host}`), opts)
let resolveReady
const readyEvents = []
const networkClient = new WSClient({ onReady: reconnect => { readyEvents.push(reconnect); resolveReady?.() } })
async function readyAfter(action) {
  let timer
  try {
    await Promise.race([
      new Promise(resolve => { resolveReady = resolve; action() }),
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('network recovery timed out')), 10_000) }),
    ])
  } finally { clearTimeout(timer); resolveReady = null }
}
try {
  await readyAfter(() => networkClient.connect())
  assert.equal((await networkClient.request('ping', {})).type, 'pong')
  await readyAfter(() => { unavailable = true; for (const peer of peers) peer.destroy() })
  assert.deepEqual(readyEvents, [false, true])
  assert.equal(outages, 1)
  assert.equal((await networkClient.request('ping', {})).type, 'pong')
  console.log('PASS real HTTP/WebSocket: dropped connection → ticket 503 → automatic reconnect → usable requests')
} finally {
  networkClient.dispose()
  for (const peer of peers) peer.destroy()
  server.closeAllConnections()
  await new Promise(resolve => server.close(resolve))
  globalThis.fetch = original.fetch
  globalThis.location = original.location
}
