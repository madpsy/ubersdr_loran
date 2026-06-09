// loran_c.js — Loran-C scope renderer for ubersdr_loran
//
// All chain metadata is fetched from GET /api/chains and GET /api/config.
// The WebSocket carries only binary scope frames.
// Zero hardcoded domain knowledge in this file.

'use strict';

const BASE_PATH = (typeof window.BASE_PATH === 'string') ? window.BASE_PATH : '';

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const CMD_SCOPE_DATA  = 0;
const CMD_SCOPE_RESET = 1;

const SCOPE_START_X = 130;  // pixels reserved for left legend
const H_LEGEND      = 20;   // pixels for emission-delay bar at bottom of each row
const ROW_HEIGHT    = 100;  // total pixels per channel row

// Station colour palette (M=red, W/X/Y/Z cycle through the rest)
const STATION_COLORS = ['#e74c3c', '#f1c40f', '#2ecc71', '#3498db', '#95a5a6'];

// One distinct trace colour per channel row
const TRACE_COLORS = [
    '#00d4ff', '#b388ff', '#ff9900', '#00ff88', '#ff4488',
    '#44aaff', '#ffff44', '#ff6644', '#88ff44', '#44ffee',
    '#cc88ff', '#ff88cc', '#88ccff', '#ffcc44',
];

// ---------------------------------------------------------------------------
// Runtime state — populated from /api/chains and /api/config
// ---------------------------------------------------------------------------

let chains   = [];   // Chain[] from /api/chains
let config   = null; // configResponse from /api/config
let msPerBin = 0;
let NCH      = 0;

// nbuckets per channel (updated when first data arrives)
let nbuckets = [];

// ---------------------------------------------------------------------------
// Canvas state
// ---------------------------------------------------------------------------

let ws = null;
let canvas, ctx;
let scopeWidth = 0;

// ---------------------------------------------------------------------------
// Layout helpers
// ---------------------------------------------------------------------------

function rowTop(i)    { return i * ROW_HEIGHT; }
function rowBottom(i) { return (i + 1) * ROW_HEIGHT - H_LEGEND; }
function scopeH()     { return ROW_HEIGHT - H_LEGEND; }

// ---------------------------------------------------------------------------
// Bootstrap — fetch config then connect
// ---------------------------------------------------------------------------

async function bootstrap() {
    try {
        const [chainsResp, configResp] = await Promise.all([
            fetch(BASE_PATH + '/api/chains'),
            fetch(BASE_PATH + '/api/config'),
        ]);

        if (!chainsResp.ok) throw new Error('/api/chains ' + chainsResp.status);
        if (!configResp.ok) throw new Error('/api/config ' + configResp.status);

        chains   = await chainsResp.json();
        config   = await configResp.json();
        msPerBin = config.ms_per_bin;
        NCH      = chains.length;
        nbuckets = new Array(NCH).fill(0);

        resizeCanvas();
        window.addEventListener('resize', resizeCanvas);
        canvas.addEventListener('click', onCanvasClick);

        connect();
    } catch (err) {
        console.error('bootstrap failed:', err);
        document.getElementById('status').textContent = 'Config error: ' + err.message;
        document.getElementById('status').className = 'status-err';
        // Retry after 5 s
        setTimeout(bootstrap, 5000);
    }
}

// ---------------------------------------------------------------------------
// WebSocket — binary scope frames only
// ---------------------------------------------------------------------------

function connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}${BASE_PATH}/ws`);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
        document.getElementById('status').textContent = 'Connected';
        document.getElementById('status').className = 'status-ok';
        sendWS({ type: 'start' });
        // Register every chain with its channel index
        for (let i = 0; i < NCH; i++) {
            const gri = chains[i].gri;
            // GRI 5991 (US west coast eLoran test) shares the 5990 transmitter
            sendWS({ type: 'set_gri', ch: i, gri: gri === 5991 ? 5990 : gri });
        }
    };

    ws.onclose = () => {
        document.getElementById('status').textContent = 'Disconnected — reconnecting…';
        document.getElementById('status').className = 'status-err';
        setTimeout(connect, 3000);
    };

    ws.onerror = (e) => console.error('WS error', e);

    ws.onmessage = (evt) => {
        if (evt.data instanceof ArrayBuffer) {
            handleBinary(evt.data);
        } else {
            // Text frame: server may push updated ms_per_bin on reconnect
            try {
                const msg = JSON.parse(evt.data);
                if (msg.type === 'ms_per_bin') {
                    msPerBin = parseFloat(msg.ms_per_bin);
                    for (let i = 0; i < NCH; i++) drawLegend(i);
                }
            } catch(e) { /* ignore */ }
        }
    };
}

function sendWS(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

// ---------------------------------------------------------------------------
// Binary frame: "DAT" + cmd(1) + ch(1) + scope_bytes
// ---------------------------------------------------------------------------

function handleBinary(buf) {
    const ba = new Uint8Array(buf);
    if (ba[0] !== 0x44 || ba[1] !== 0x41 || ba[2] !== 0x54) return; // "DAT"
    const cmd   = ba[3];
    const chIdx = ba[4];
    const data  = ba.slice(5);
    if (chIdx < 0 || chIdx >= NCH) return;
    nbuckets[chIdx] = data.length;
    drawScope(chIdx, cmd, data);
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

function drawScope(chIdx, cmd, data) {
    if (!canvas) return;
    const sx    = SCOPE_START_X;
    const yb    = rowBottom(chIdx);
    const yh    = scopeH();
    const color = TRACE_COLORS[chIdx % TRACE_COLORS.length];
    const BG    = '#08080c';

    if (cmd === CMD_SCOPE_RESET) {
        ctx.fillStyle = BG;
        ctx.fillRect(sx, rowTop(chIdx), scopeWidth - sx, yh);
        drawLegend(chIdx);
    }

    const blen = data.length;

    // Pass 1 — clear all columns to background
    ctx.fillStyle = BG;
    ctx.fillRect(sx, rowTop(chIdx), blen, yh);

    // Pass 2 — wide soft glow (5px)
    ctx.globalAlpha = 0.18;
    ctx.fillStyle = color;
    for (let i = 0; i < blen; i++) {
        const z = data[i] / 255;
        if (z > 0.01) {
            const h = z * yh;
            ctx.fillRect(sx + i - 2, yb - h, 5, h);
        }
    }

    // Pass 3 — medium glow (3px)
    ctx.globalAlpha = 0.35;
    for (let i = 0; i < blen; i++) {
        const z = data[i] / 255;
        if (z > 0.01) {
            const h = z * yh;
            ctx.fillRect(sx + i - 1, yb - h, 3, h);
        }
    }

    // Pass 4 — solid 1px trace at full brightness
    ctx.globalAlpha = 1.0;
    for (let i = 0; i < blen; i++) {
        const z = data[i] / 255;
        if (z > 0.01) {
            const h = Math.max(z * yh, 1);
            ctx.fillRect(sx + i, yb - h, 1, h);
        }
    }

    // Pass 5 — bright 1px tip at the peak of each column
    ctx.fillStyle = 'white';
    ctx.globalAlpha = 0.7;
    for (let i = 0; i < blen; i++) {
        const z = data[i] / 255;
        if (z > 0.05) {
            ctx.fillRect(sx + i, yb - Math.ceil(z * yh), 1, 1);
        }
    }

    ctx.globalAlpha = 1.0;

    // Clear right of data
    ctx.fillStyle = BG;
    ctx.fillRect(sx + blen, rowTop(chIdx), scopeWidth - sx - blen + 1, yh);
}

function drawLegend(chIdx) {
    if (!canvas || chIdx >= chains.length) return;
    const chain = chains[chIdx];
    const color = TRACE_COLORS[chIdx % TRACE_COLORS.length];
    const sx    = SCOPE_START_X;
    const yb    = rowBottom(chIdx);
    const yt    = rowTop(chIdx);

    // ── Left margin ──────────────────────────────────────────
    ctx.fillStyle = '#08080c';
    ctx.fillRect(0, yt, sx - 1, ROW_HEIGHT);

    // Thin left accent bar
    ctx.fillStyle = color;
    ctx.fillRect(0, yt + 2, 3, ROW_HEIGHT - 4);

    // GRI number — bold, trace colour
    ctx.font = 'bold 11px "Inter", system-ui, sans-serif';
    ctx.fillStyle = color;
    ctx.fillText('GRI ' + chain.gri, 7, yt + 14);

    // Chain name lines — muted
    ctx.font = '10px "Inter", system-ui, sans-serif';
    ctx.fillStyle = '#808090';
    ctx.fillText(chain.short[0], 7, yt + 27);
    if (chain.short[1]) ctx.fillText(chain.short[1], 7, yt + 39);

    // ── Emission-delay bar ───────────────────────────────────
    ctx.fillStyle = '#08080c';
    ctx.fillRect(sx, yb, scopeWidth - sx, H_LEGEND);

    if (msPerBin === 0 || !chain.stations || chain.stations.length === 0) return;

    const stations = chain.stations;
    for (let i = 0; i < stations.length; i++) {
        const st  = stations[i];
        const next = stations[i + 1];

        let ed0 = Math.round((st.delay_us / 1000) / msPerBin);
        let ed1 = next
            ? Math.round((next.delay_us / 1000) / msPerBin)
            : Math.round((chain.gri / 100));  // end of GRI period in buckets

        ctx.fillStyle = STATION_COLORS[i % STATION_COLORS.length];
        ctx.globalAlpha = 0.85;
        ctx.fillRect(sx + ed0, yb + 1, Math.max(ed1 - ed0 - 1, 1), 5);
        ctx.globalAlpha = 1.0;

        ctx.font = '9px "Inter", system-ui, sans-serif';
        ctx.fillStyle = '#c0c0cc';
        ctx.fillText(st.id + ' ' + st.name, sx + ed0 + 2, yb + H_LEGEND - 4);
    }
}

// ---------------------------------------------------------------------------
// Canvas click — align master station for the clicked row
// ---------------------------------------------------------------------------

function onCanvasClick(evt) {
    if (!canvas) return;
    const rect   = canvas.getBoundingClientRect();
    const scaleY = canvas.height / rect.height;
    const scaleX = canvas.width  / rect.width;
    const y      = (evt.clientY - rect.top)  * scaleY;
    const x      = (evt.clientX - rect.left) * scaleX;
    const chIdx  = Math.floor(y / ROW_HEIGHT);
    if (chIdx < 0 || chIdx >= NCH) return;
    const offset = Math.round(x) - SCOPE_START_X;
    if (offset < 0 || offset >= nbuckets[chIdx]) return;
    sendWS({ type: 'set_offset', ch: chIdx, offset: offset });
}

// ---------------------------------------------------------------------------
// Resize
// ---------------------------------------------------------------------------

function resizeCanvas() {
    if (!canvas || NCH === 0) return;
    const w = document.getElementById('scope-container').clientWidth;
    scopeWidth    = w;
    canvas.width  = w;
    canvas.height = NCH * ROW_HEIGHT;

    ctx.fillStyle = '#08080c';
    ctx.fillRect(0, 0, w, canvas.height);

    for (let i = 0; i < NCH; i++) {
        if (i > 0) {
            ctx.strokeStyle = '#22222a';
            ctx.lineWidth = 1;
            ctx.beginPath();
            ctx.moveTo(0, rowTop(i) - 0.5);
            ctx.lineTo(w, rowTop(i) - 0.5);
            ctx.stroke();
        }
        drawLegend(i);
    }
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

function init() {
    canvas = document.getElementById('scope');
    ctx    = canvas.getContext('2d');
    bootstrap();
}

document.addEventListener('DOMContentLoaded', init);
