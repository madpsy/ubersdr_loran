// loran_c.js — Loran-C scope renderer for ubersdr_loran
//
// All GRI_LIST chains are decoded and displayed simultaneously — one row each.
// There are no per-channel controls; every chain is always on.

'use strict';

const BASE_PATH = (typeof window.BASE_PATH === 'string') ? window.BASE_PATH : '';

// ---------------------------------------------------------------------------
// GRI chain table
// ---------------------------------------------------------------------------

const GRI_LIST = [
    { gri: 5960, name: 'North Russia (Chayka)',   short: ['North Russia',   '(Chayka)'] },
    { gri: 5990, name: 'Caucasus',                short: ['Caucasus',       ''] },
    { gri: 5991, name: 'USA west coast (eLoran)', short: ['USA west coast', '(eLoran)'] },
    { gri: 6000, name: 'China BPL Pucheng',       short: ['China BPL',      'Pucheng'] },
    { gri: 6731, name: 'Anthorn UK',              short: ['Anthorn UK',     ''] },
    { gri: 6780, name: 'China South Sea',         short: ['China Sea',      'South'] },
    { gri: 7430, name: 'China North Sea',         short: ['China Sea',      'North'] },
    { gri: 7950, name: 'Eastern Russia (Chayka)', short: ['Eastern Russia', '(Chayka)'] },
    { gri: 8000, name: 'Western Russia (Chayka)', short: ['Western Russia', '(Chayka)'] },
    { gri: 8390, name: 'China East Sea',          short: ['China Sea',      'East'] },
    { gri: 8830, name: 'Saudi Arabia North',      short: ['Saudi Arabia',   'North'] },
    { gri: 8970, name: 'USA east coast (eLoran)', short: ['USA east coast', '(eLoran)'] },
    { gri: 9930, name: 'Korea',                   short: ['Korea',          ''] },
    { gri: 9960, name: 'USA east coast (eLoran)', short: ['USA east coast', '(eLoran)'] },
];

const NCH = GRI_LIST.length; // must match nch in decoder.go

const EMISSION_DELAY = {
    5960: [{ s:'M Inta', d:0 }, { s:'X Tumanny Pen', d:14670.15 }, { s:'Z Norilsk', d:45915.33 }],
    5990: [{ s:'M Caucasian Center', d:0 }, { s:'X Caucasian West', d:16587 }, { s:'Y Caucasian East', d:31304 }, { s:'Z Caucasian North', d:46440 }],
    5991: [{ s:'M George | Variable: Fallon, Havre', d:0 }],
    6000: [{ s:'M Pucheng', d:0 }],
    6731: [{ s:'M Anthorn', d:0 }, { s:'Y Anthorn', d:27300.00 }],
    6780: [{ s:'M Hexian', d:0 }, { s:'X Raoping', d:14464.69 }, { s:'Y Chongzuo', d:26925.76 }],
    7430: [{ s:'M Rongcheng', d:0 }, { s:'X Xuancheng', d:13459.70 }, { s:'Y Helong', d:30852.32 }],
    7950: [{ s:'M Aleksandrovsk', d:0 }, { s:'W Petropavlovsk', d:14506.50 }, { s:'X Ussuriisk', d:33678.00 }, { s:'Y (ex-Tokachibuto)', d:49104.15 }, { s:'Z Okhotsk', d:64102.05 }],
    8000: [{ s:'M Bryansk', d:0 }, { s:'W Petrozavodsk', d:13217.21 }, { s:'X Slonim', d:27125.00 }, { s:'Y Simferopol', d:53070.25 }, { s:'Z Syzran', d:67941.60 }],
    8390: [{ s:'M Xuancheng', d:0 }, { s:'X Raoping', d:13795.52 }, { s:'Y Rongcheng', d:31459.70 }],
    8830: [{ s:'M Afif', d:0 }, { s:'W Salwa', d:13645.00 }, { s:'X (ex-Al Khamasin)', d:27265.00 }, { s:'Y Ash Shaykh', d:42645.00 }, { s:'Z Al Muwassam', d:58790.00 }],
    8970: [{ s:'M Wildwood', d:0 }],
    9930: [{ s:'M Pohang', d:0 }, { s:'W Kwang Ju', d:11946.97 }, { s:'X (ex-Gesashi)', d:25565.52 }, { s:'Y (ex-Niijima)', d:40085.64 }, { s:'Z Ussuriisk', d:54162.44 }],
    9960: [{ s:'M Wildwood', d:0 }],
};

const STATION_COLORS = ['red', 'yellow', 'lime', 'blue', 'grey'];

// One distinct trace colour per channel row
const TRACE_COLORS = [
    'cyan', 'violet', '#ff9900', '#00ff88', '#ff4488',
    '#44aaff', '#ffff44', '#ff6644', '#88ff44', '#44ffee',
    '#cc88ff', '#ff88cc', '#88ccff', '#ffcc44',
];

const CMD_SCOPE_DATA  = 0;
const CMD_SCOPE_RESET = 1;

const SCOPE_START_X = 130;  // pixels reserved for left legend
const H_LEGEND      = 20;   // pixels for emission-delay bar at bottom of each row
const ROW_HEIGHT    = 90;   // total pixels per channel row

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

let ws = null;
let msPerBin = 0;
let canvas, ctx;
let scopeWidth = 0;

// nbuckets per channel (updated when first data arrives)
const nbuckets = new Array(NCH).fill(0);

// ---------------------------------------------------------------------------
// Layout helpers
// ---------------------------------------------------------------------------

function rowTop(i)    { return i * ROW_HEIGHT; }
function rowBottom(i) { return (i + 1) * ROW_HEIGHT - H_LEGEND; } // top of legend bar
function scopeH()     { return ROW_HEIGHT - H_LEGEND; }

// ---------------------------------------------------------------------------
// WebSocket
// ---------------------------------------------------------------------------

function connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}${BASE_PATH}/ws`);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
        document.getElementById('status').textContent = 'Connected';
        document.getElementById('status').className = 'status-ok';
        sendControl({ type: 'start' });
        // Register every GRI with its channel index
        for (let i = 0; i < NCH; i++) {
            const gri = GRI_LIST[i].gri;
            // 5991 shares the 5990 transmitter on the server side
            sendControl({ type: 'set_gri', ch: i, gri: gri === 5991 ? 5990 : gri });
        }
    };

    ws.onclose = () => {
        document.getElementById('status').textContent = 'Disconnected — reconnecting…';
        document.getElementById('status').className = 'status-err';
        setTimeout(connect, 3000);
    };

    ws.onerror = (e) => console.error('WS error', e);

    ws.onmessage = (evt) => {
        if (evt.data instanceof ArrayBuffer) handleBinary(evt.data);
        else handleText(evt.data);
    };
}

function sendControl(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

// ---------------------------------------------------------------------------
// Binary frame: "DAT" + cmd(1) + ch(1) + scope_bytes
// ---------------------------------------------------------------------------

function handleBinary(buf) {
    const ba = new Uint8Array(buf);
    if (ba[0] !== 0x44 || ba[1] !== 0x41 || ba[2] !== 0x54) return;
    const cmd   = ba[3];
    const chIdx = ba[4];
    const data  = ba.slice(5);
    if (chIdx < 0 || chIdx >= NCH) return;
    nbuckets[chIdx] = data.length;
    drawScope(chIdx, cmd, data);
}

// ---------------------------------------------------------------------------
// Text frame
// ---------------------------------------------------------------------------

function handleText(raw) {
    let msg;
    try { msg = JSON.parse(raw); } catch(e) { return; }
    if (msg.type === 'ms_per_bin') {
        msPerBin = parseFloat(msg.ms_per_bin);
        for (let i = 0; i < NCH; i++) drawLegend(i);
    }
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

    if (cmd === CMD_SCOPE_RESET) {
        ctx.fillStyle = '#050507';
        ctx.fillRect(sx, rowTop(chIdx), scopeWidth - sx, yh);
        drawLegend(chIdx);
    }

    const blen = data.length;
    for (let i = 0; i < blen; i++) {
        const z = data[i] / 255;
        // Background column
        ctx.fillStyle = '#050507';
        ctx.fillRect(sx + i, yb, 1, -yh);
        if (z > 0) {
            // Glow: faint wide bar behind the trace
            ctx.fillStyle = color;
            ctx.globalAlpha = 0.12;
            ctx.fillRect(sx + i, yb, 1, z * -yh);
            // Solid trace
            ctx.globalAlpha = 0.9;
            ctx.fillRect(sx + i, yb, 1, Math.max(z * -yh, -1));
            ctx.globalAlpha = 1.0;
        }
    }
    // Clear right of data
    ctx.fillStyle = '#050507';
    ctx.fillRect(sx + blen, yb, scopeWidth - sx - blen + 1, -yh);
}

function drawLegend(chIdx) {
    if (!canvas) return;
    const gri   = GRI_LIST[chIdx].gri;
    const entry = GRI_LIST[chIdx];
    const color = TRACE_COLORS[chIdx % TRACE_COLORS.length];
    const sx    = SCOPE_START_X;
    const yb    = rowBottom(chIdx);   // top of legend bar
    const yt    = rowTop(chIdx);

    // ── Left margin ──────────────────────────────────────────
    // Subtle tinted background matching the trace colour
    ctx.fillStyle = '#0d0d0f';
    ctx.fillRect(0, yt, sx - 1, ROW_HEIGHT);

    // Thin left accent bar
    ctx.fillStyle = color;
    ctx.globalAlpha = 0.7;
    ctx.fillRect(0, yt + 2, 3, ROW_HEIGHT - 4);
    ctx.globalAlpha = 1.0;

    // GRI number — bold, trace colour
    ctx.font = 'bold 11px "Inter", system-ui, sans-serif';
    ctx.fillStyle = color;
    ctx.fillText('GRI ' + gri, 7, yt + 14);

    // Chain name lines — muted white
    ctx.font = '10px "Inter", system-ui, sans-serif';
    ctx.fillStyle = '#9090a0';
    ctx.fillText(entry.short[0], 7, yt + 27);
    if (entry.short[1]) ctx.fillText(entry.short[1], 7, yt + 39);

    // ── Emission-delay bar ───────────────────────────────────
    ctx.fillStyle = '#0a0a0c';
    ctx.fillRect(sx, yb, scopeWidth - sx, H_LEGEND);

    if (msPerBin === 0) return;
    const ed = EMISSION_DELAY[gri];
    if (!ed) return;

    for (let i = 0; i < ed.length; i++) {
        let ed0 = Math.round((ed[i].d / 1000) / msPerBin);
        let ed1 = Math.round(((i < ed.length - 1 ? ed[i + 1].d : gri * 10) / 1000) / msPerBin);

        // Coloured segment
        ctx.fillStyle = STATION_COLORS[i % STATION_COLORS.length];
        ctx.globalAlpha = 0.85;
        ctx.fillRect(sx + ed0, yb + 1, Math.max(ed1 - ed0 - 1, 1), 5);
        ctx.globalAlpha = 1.0;

        // Station label
        ctx.font = '9px "Inter", system-ui, sans-serif';
        ctx.fillStyle = '#c0c0cc';
        ctx.fillText(ed[i].s, sx + ed0 + 2, yb + H_LEGEND - 4);
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
    sendControl({ type: 'set_offset', ch: chIdx, offset: offset });
}

// ---------------------------------------------------------------------------
// Resize
// ---------------------------------------------------------------------------

function resizeCanvas() {
    if (!canvas) return;
    const w = document.getElementById('scope-container').clientWidth;
    scopeWidth    = w;
    canvas.width  = w;
    canvas.height = NCH * ROW_HEIGHT;

    // Base fill
    ctx.fillStyle = '#050507';
    ctx.fillRect(0, 0, w, canvas.height);

    for (let i = 0; i < NCH; i++) {
        // Subtle alternating row tint
        if (i % 2 === 1) {
            ctx.fillStyle = 'rgba(255,255,255,0.015)';
            ctx.fillRect(0, rowTop(i), w, ROW_HEIGHT);
        }
        // Row separator
        if (i > 0) {
            ctx.strokeStyle = '#1e1e24';
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
    resizeCanvas();
    window.addEventListener('resize', resizeCanvas);
    canvas.addEventListener('click', onCanvasClick);
    connect();
}

document.addEventListener('DOMContentLoaded', init);
