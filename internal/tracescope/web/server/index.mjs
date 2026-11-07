import { createServer } from 'node:http'
import { readFile, stat } from 'node:fs/promises'
import { extname, join, normalize } from 'node:path'
import { fileURLToPath } from 'node:url'
import { WebSocketServer, WebSocket } from 'ws'
import { aggregateConnections } from './lib/network.mjs'
import { ConnectionCollector } from './lib/collector.mjs'
import { RouteEnricher } from './lib/enricher.mjs'
import { loadPhysicalOrigin } from './lib/origin.mjs'
import { message } from './lib/schema.mjs'
import { loadMaxRoutes } from './lib/settings.mjs'

// >>> vendored patch (tizi): serve the built SPA so one process hosts page + API.
const WEB_ROOT = process.env.TRACESCOPE_WEB_ROOT || join(fileURLToPath(new URL('.', import.meta.url)), '..', 'dist')
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json; charset=utf-8', '.svg': 'image/svg+xml', '.png': 'image/png', '.jpg': 'image/jpeg', '.webp': 'image/webp', '.woff': 'font/woff', '.woff2': 'font/woff2', '.ico': 'image/x-icon', '.map': 'application/json' }
async function serveStatic(request, response) {
  if (request.method !== 'GET' && request.method !== 'HEAD') { response.writeHead(405).end('Method Not Allowed'); return }
  const url = new URL(request.url, `http://${request.headers.host || '127.0.0.1'}`)
  let rel = normalize(decodeURIComponent(url.pathname)).replace(/^(\.\.[/\\])+/, '').replace(/^[/\\]+/, '')
  if (!rel || rel === '.' || rel.endsWith('/')) rel = 'index.html'
  const file = join(WEB_ROOT, rel)
  if (!file.startsWith(WEB_ROOT)) { response.writeHead(403).end('Forbidden'); return }
  try {
    let target = file
    let info = await stat(target)
    if (info.isDirectory()) { target = join(target, 'index.html'); info = await stat(target) }
    const ext = extname(target).toLowerCase()
    response.writeHead(200, { 'Content-Type': MIME[ext] || 'application/octet-stream', 'Cache-Control': ext === '.html' ? 'no-store' : 'public, max-age=3600' })
    if (request.method === 'HEAD') { response.end(); return }
    response.end(await readFile(target))
  } catch {
    // SPA fallback: unknown paths render the app shell.
    try {
      response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' })
      response.end(await readFile(join(WEB_ROOT, 'index.html')))
    } catch {
      response.writeHead(404).end('Not found')
    }
  }
}
// <<< vendored patch

const HOST = '127.0.0.1'
const PORT = Number(process.env.TRACESCOPE_PORT || 8788)
const POLL_MS = 1_500
const STALE_MS = 10_000
const MAX_ROUTES = loadMaxRoutes()
// >>> vendored patch: also allow the port this page is served from.
const allowedOrigins = new Set([
  'http://127.0.0.1:4173', 'http://localhost:4173',
  'http://127.0.0.1:5173', 'http://localhost:5173',
  `http://127.0.0.1:${PORT}`, `http://localhost:${PORT}`,
])
const allowedOrigin = (origin) => !origin || allowedOrigins.has(origin)
// <<< vendored patch
const collector = new ConnectionCollector()
const physicalOrigin = loadPhysicalOrigin()
const clients = new Set()
let selected = []
let latestRoutes = []
let firstSnapshot = true
let publishTimer
let status = { source: 'none', collected: 0, publicTargets: 0, privateFiltered: 0, displayed: 0, maxRoutes: MAX_ROUTES, geoProvider: 'ipwho.is', updatedAt: new Date().toISOString() }
const history = new Map()

function broadcast(payload) {
  const body = JSON.stringify(payload)
  for (const client of clients) if (client.readyState === WebSocket.OPEN) client.send(body)
}

function publish() {
  latestRoutes = selected.map((connection) => enricher.get(connection)).filter(Boolean)
  status = { ...status, displayed: latestRoutes.length, updatedAt: new Date().toISOString() }
  broadcast(message(firstSnapshot ? 'snapshot' : 'update', { status, routes: latestRoutes }))
  firstSnapshot = false
}

function schedulePublish() {
  clearTimeout(publishTimer)
  publishTimer = setTimeout(publish, 80)
}

const enricher = new RouteEnricher(schedulePublish, { physicalOrigin })

function updateHistory(groups, nowMs) {
  const seen = new Set()
  for (const group of groups) {
    seen.add(group.id)
    const previous = history.get(group.id)
    const bytes = group.upload + group.download
    const changed = !previous || bytes !== previous.bytes
    history.set(group.id, { ...group, bytes, delta: previous ? Math.max(0, bytes - previous.bytes) : bytes, lastSeen: changed ? new Date(nowMs).toISOString() : previous.lastSeen, stale: false, missingSince: undefined })
  }
  for (const [id, previous] of history) {
    if (seen.has(id)) continue
    const missingSince = previous.missingSince || nowMs
    if (nowMs - missingSince > STALE_MS) history.delete(id)
    else history.set(id, { ...previous, delta: 0, stale: true, missingSince })
  }
  return [...history.values()].sort((a, b) => Number(a.stale) - Number(b.stale) || b.delta - a.delta || Date.parse(b.lastSeen) - Date.parse(a.lastSeen) || b.bytes - a.bytes).slice(0, MAX_ROUTES)
}

async function poll() {
  try {
    const sample = await collector.collect()
    const groups = aggregateConnections(sample.connections)
    selected = updateHistory(groups, Date.now())
    status = { source: sample.source, collected: sample.collected, publicTargets: groups.length, privateFiltered: sample.privateFiltered, displayed: latestRoutes.length, maxRoutes: MAX_ROUTES, geoProvider: 'ipwho.is', updatedAt: new Date().toISOString() }
    broadcast(message('status', { status }))
    publish()
  } catch {
    broadcast(message('error', { code: 'COLLECTOR_UNAVAILABLE', message: '本机连接采集暂时不可用', recoverable: true }))
  }
}

const server = createServer(async (request, response) => {
  const origin = request.headers.origin
  if (origin && !allowedOrigin(origin)) {
    response.writeHead(403).end('Forbidden')
    return
  }
  if (request.url === '/health') {
    response.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' })
    response.end(JSON.stringify({ ok: true, source: status.source, collected: status.collected, publicTargets: status.publicTargets, displayed: status.displayed, maxRoutes: MAX_ROUTES, physicalOrigin: physicalOrigin.city, updatedAt: status.updatedAt }))
    return
  }
  // >>> vendored patch: serve the built SPA (with SPA fallback) instead of 404.
  try {
    await serveStatic(request, response)
  } catch {
    response.writeHead(500).end('Internal error')
  }
  // <<< vendored patch
})

const wss = new WebSocketServer({ noServer: true })
wss.on('connection', (socket) => {
  socket.isAlive = true
  clients.add(socket)
  socket.on('pong', () => { socket.isAlive = true })
  socket.on('close', () => clients.delete(socket))
  socket.send(JSON.stringify(message('status', { status })))
  socket.send(JSON.stringify(message('snapshot', { status, routes: latestRoutes })))
})

server.on('upgrade', (request, socket, head) => {
  const origin = request.headers.origin
  if (request.url !== '/ws' || !allowedOrigin(origin)) {
    socket.write('HTTP/1.1 403 Forbidden\r\n\r\n')
    socket.destroy()
    return
  }
  wss.handleUpgrade(request, socket, head, (client) => wss.emit('connection', client, request))
})

const heartbeat = setInterval(() => {
  for (const socket of clients) {
    if (!socket.isAlive) {
      socket.terminate()
      continue
    }
    socket.isAlive = false
    socket.ping()
  }
}, 30_000)

server.listen(PORT, HOST, () => {
  console.log(`TraceScope local feed listening on http://${HOST}:${PORT}`)
  void poll()
})
const poller = setInterval(() => void poll(), POLL_MS)

function shutdown() {
  clearInterval(poller)
  clearInterval(heartbeat)
  clearTimeout(publishTimer)
  for (const socket of clients) socket.close(1001, 'Server shutdown')
  server.close(() => process.exit(0))
}
process.on('SIGINT', shutdown)
process.on('SIGTERM', shutdown)

// >>> vendored patch (tizi): exit when the launching parent is gone.
// tizi sets TRACESCOPE_PARENT_PID; if that process dies without
// signalling us (e.g. SIGKILL), keep the port from being held by an orphan.
const PARENT_PID = Number(process.env.TRACESCOPE_PARENT_PID || 0)
if (Number.isInteger(PARENT_PID) && PARENT_PID > 1) {
  const parentAlive = () => {
    try { process.kill(PARENT_PID, 0); return true } catch (error) { return error.code === 'EPERM' }
  }
  const parentWatch = setInterval(() => {
    if (parentAlive()) return
    clearInterval(parentWatch)
    console.log(`parent process ${PARENT_PID} exited; shutting down`)
    shutdown()
  }, 2_000)
  parentWatch.unref()
}
// <<< vendored patch
