// loran_c.js — Loran-C scope renderer for ubersdr_loran
//
// All chain metadata is fetched from GET /api/chains and GET /api/config.
// The WebSocket carries binary scope frames and JSON push messages.
// Zero hardcoded domain knowledge in this file.
//
// Exposes to other scripts:
//   window.loranChains        — Chain[] from /api/chains (set after bootstrap)
//   window.loranTraceColors   — colour palette (set immediately)
//   window.loranWsSubscribe(fn) — register a JSON WS message handler

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

// Expose palette for map.js / tdoa_panel.js
window.loranTraceColors = TRACE_COLORS;

// SNR thresholds for badge colouring (dB)
const SNR_GOOD = 15;
const SNR_WARN  = 8;

// ---------------------------------------------------------------------------
// Runtime state — populated from /api/chains and /api/config
// ---------------------------------------------------------------------------

let chains   = [];   // Chain[] from /api/chains
let config   = null; // configResponse from /api/config
let msPerBin = 0;
let NCH      = 0;

// nbuckets per channel (updated when first data arrives)
let nbuckets = [];

// Per-channel quality (from quality_update WS messages)
// key = chIdx, value = { snr, peak_bin, ... }
const channelQuality = {};

// Per-channel TDOA measurements (from tdoa_update WS messages)
// key = chIdx, value = TDOAMeasurement[]
const channelTDOA = {};

// ---------------------------------------------------------------------------
// Canvas state
// ---------------------------------------------------------------------------

let ws = null;
let canvas, ctx;
let scopeWidth = 0;

// ---------------------------------------------------------------------------
// WS JSON subscriber list
// ---------------------------------------------------------------------------

const wsSubscribers = [];

/**
 * Register a callback to receive JSON WebSocket messages.
 * Called by map.js and tdoa_panel.js.
 */
window.loranWsSubscribe = function (fn) {
    wsSubscribers.push(fn);
};

function dispatchWsMessage(msg) {
    wsSubscribers.forEach(fn => {
        try { fn(msg); } catch (e) { console.error('WS subscriber error', e); }
    });
}

// ---------------------------------------------------------------------------
// Layout helpers
// ---------------------------------------------------------------------------

function rowTop(i)    { return i * ROW_HEIGHT; }
function rowBottom(i) { return (i + 1) * ROW_HEIGHT - H_LEGEND; }
function scopeH()     { return ROW_HEIGHT - H_LEGEND; }

// ---------------------------------------------------------------------------
// UTC clock — driven by timing_update WS messages from the server.
// Between WS pushes the display is advanced by a local JS timer so it ticks
// smoothly every 100 ms without any REST polling.
// ---------------------------------------------------------------------------

let utcOffsetMs = 0;      // difference: server UTC ms - Date.now() at last push
let utcValid    = false;  // true once first timing_update arrives
let utcTicker   = null;   // setInterval handle for the display tick

function applyTimingUpdate(data) {
    if (!data.valid) return;
    // data.utc is RFC3339, e.g. "2026-06-09T11:45:00.123Z"
    const serverMs = new Date(data.utc).getTime();
    utcOffsetMs = serverMs - Date.now();
    utcValid    = true;
    updateUtcDisplay();
}

function updateUtcDisplay() {
    const el = document.getElementById('utc-time');
    if (!el) return;
    if (!utcValid) { el.textContent = '--:--:--.---'; return; }
    const now = new Date(Date.now() + utcOffsetMs);
    const hh  = String(now.getUTCHours()).padStart(2, '0');
    const mm  = String(now.getUTCMinutes()).padStart(2, '0');
    const ss  = String(now.getUTCSeconds()).padStart(2, '0');
    const ms  = String(now.getUTCMilliseconds()).padStart(3, '0');
    el.textContent = `${hh}:${mm}:${ss}.${ms}`;
}

function startUtcClock() {
    // Tick the display every 100 ms for smooth millisecond updates.
    // Actual time sync comes from timing_update WS messages.
    utcTicker = setInterval(updateUtcDisplay, 100);
}

// ---------------------------------------------------------------------------
// "Heard only" filter — driven by #valid-only checkbox
// ---------------------------------------------------------------------------

function heardOnly() {
    const cb = document.getElementById('valid-only');
    return cb ? cb.checked : false;
}

// Returns true if channel chIdx has a master SNR above the warning threshold
// (i.e. we can actually hear it).
function isChannelHeard(chIdx) {
    const q = channelQuality[chIdx];
    if (!q || q.snr_db === undefined) return false;
    return q.snr_db >= SNR_WARN;
}

// Re-draw all legends (called when checkbox toggles)
function redrawAllLegends() {
    for (let i = 0; i < NCH; i++) drawLegend(i);
}

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

        // Expose chains for map.js / tdoa_panel.js
        window.loranChains = chains;

        resizeCanvas();
        window.addEventListener('resize', resizeCanvas);
        canvas.addEventListener('click', onCanvasClick);

        // Wire "heard only" checkbox — redraw all legends on toggle
        const validOnlyCb = document.getElementById('valid-only');
        if (validOnlyCb) {
            validOnlyCb.addEventListener('change', redrawAllLegends);
        }

        connect();

        // Start UTC clock display (ticks locally; synced via timing_update WS push)
        startUtcClock();

        // Initialise sibling modules after chains are available
        if (typeof window.mapInit === 'function') {
            window.mapInit(chains, TRACE_COLORS);
        }
        if (typeof window.tdoaPanelInit === 'function') {
            window.tdoaPanelInit();
        }
    } catch (err) {
        console.error('bootstrap failed:', err);
        document.getElementById('status').textContent = 'Config error: ' + err.message;
        document.getElementById('status').className = 'status-err';
        // Retry after 5 s
        setTimeout(bootstrap, 5000);
    }
}

// ---------------------------------------------------------------------------
// WebSocket — binary scope frames + JSON push messages
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
            try {
                const msg = JSON.parse(evt.data);
                handleJsonMessage(msg);
            } catch(e) { /* ignore malformed */ }
        }
    };
}

function sendWS(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

// ---------------------------------------------------------------------------
// JSON message dispatcher
// ---------------------------------------------------------------------------

function handleJsonMessage(msg) {
    switch (msg.type) {
        case 'ms_per_bin':
            msPerBin = parseFloat(msg.ms_per_bin);
            for (let i = 0; i < NCH; i++) drawLegend(i);
            break;

        case 'timing_update':
            applyTimingUpdate(msg);
            break;

        case 'quality_update': {
            // Server may push {channels: [...]} or {quality: [...]}
            const qArr = msg.channels || msg.quality;
            if (Array.isArray(qArr)) {
                qArr.forEach(q => {
                    if (q.ch_idx !== undefined) {
                        channelQuality[q.ch_idx] = q;
                    }
                });
                // Redraw all legends — SNR change may affect dim overlay
                for (let i = 0; i < NCH; i++) {
                    if (channelQuality[i] !== undefined) drawLegend(i);
                }
            }
            break;
        }

        case 'tdoa_update': {
            // measurements is TDOAMeasurement[] — keyed by chain_gri, not ch_idx
            const mArr = msg.measurements;
            if (Array.isArray(mArr)) {
                mArr.forEach(m => {
                    // Map chain_gri → ch_idx via chains array
                    const chIdx = chains.findIndex(c => c.gri === m.chain_gri);
                    if (chIdx < 0) return;
                    if (!channelTDOA[chIdx]) channelTDOA[chIdx] = [];
                    const arr = channelTDOA[chIdx];
                    const idx = arr.findIndex(x => x.secondary_id === m.secondary_id);
                    if (idx >= 0) arr[idx] = m; else arr.push(m);
                });
                // Redraw peak markers for all affected channels
                const affected = new Set(
                    mArr.map(m => chains.findIndex(c => c.gri === m.chain_gri))
                        .filter(i => i >= 0)
                );
                affected.forEach(ch => { if (ch < NCH) drawPeakMarkers(ch); });
            }
            break;
        }
    }

    // Forward to all subscribers (map.js, tdoa_panel.js)
    dispatchWsMessage(msg);
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
    // Redraw peak markers on top of fresh trace
    drawPeakMarkers(chIdx);
}

// ---------------------------------------------------------------------------
// Drawing — scope trace
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

// ---------------------------------------------------------------------------
// Drawing — peak markers (overlaid on top of trace)
// ---------------------------------------------------------------------------

function drawPeakMarkers(chIdx) {
    if (!canvas || chIdx >= chains.length) return;

    const tdoas = channelTDOA[chIdx];
    if (!tdoas || tdoas.length === 0) return;

    const sx = SCOPE_START_X;
    const yb = rowBottom(chIdx);
    const yh = scopeH();

    // Master peak — from quality data (JSON fields: snr_db, peak_bin)
    const q = channelQuality[chIdx];
    if (q && q.snr_db >= SNR_WARN && q.peak_bin > 0) {
        const x = sx + q.peak_bin;
        if (x >= sx && x < scopeWidth) {
            drawTickMark(x, yb, yh, STATION_COLORS[0], null);
        }
    }

    // Secondary peaks — from TDOA measurements
    // TDOAMeasurement JSON fields: snr_db (secondary SNR), peak_bin (secondary peak bucket)
    tdoas.forEach((m, i) => {
        if (!m.valid) return;
        if ((m.snr_db || 0) < SNR_WARN) return;
        if (!m.peak_bin || m.peak_bin <= 0) return;

        const x = sx + m.peak_bin;
        if (x < sx || x >= scopeWidth) return;

        const color = STATION_COLORS[(i + 1) % STATION_COLORS.length];
        const label = m.tdoa_us !== undefined
            ? (m.tdoa_us >= 0 ? '+' : '') + m.tdoa_us.toFixed(1) + 'µs'
            : null;
        drawTickMark(x, yb, yh, color, label);
    });
}

/**
 * Draw a vertical tick mark at canvas x position.
 * @param {number} x       - canvas x coordinate
 * @param {number} yb      - row bottom y (trace baseline)
 * @param {number} yh      - row height (trace area)
 * @param {string} color   - CSS colour string
 * @param {string|null} label - optional text label above tick
 */
function drawTickMark(x, yb, yh, color, label) {
    ctx.save();
    ctx.globalAlpha = 0.85;
    ctx.strokeStyle = color;
    ctx.lineWidth   = 1.5;
    ctx.setLineDash([3, 2]);
    ctx.beginPath();
    ctx.moveTo(x + 0.5, yb);
    ctx.lineTo(x + 0.5, yb - yh + 4);
    ctx.stroke();
    ctx.setLineDash([]);

    // Small triangle at baseline
    ctx.fillStyle = color;
    ctx.globalAlpha = 1.0;
    ctx.beginPath();
    ctx.moveTo(x - 3, yb);
    ctx.lineTo(x + 3, yb);
    ctx.lineTo(x,     yb - 6);
    ctx.closePath();
    ctx.fill();

    // Label
    if (label) {
        ctx.font = '8px "Inter", system-ui, sans-serif';
        ctx.fillStyle = color;
        ctx.globalAlpha = 0.9;
        const tw = ctx.measureText(label).width;
        // Position label to the right, or left if near right edge
        const lx = (x + tw + 4 < scopeWidth) ? x + 3 : x - tw - 3;
        ctx.fillText(label, lx, yb - yh + 12);
    }

    ctx.restore();
}

// ---------------------------------------------------------------------------
// Drawing — legend (left margin + emission-delay bar)
// ---------------------------------------------------------------------------

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

    // Chain name lines — white
    ctx.font = '10px "Inter", system-ui, sans-serif';
    ctx.fillStyle = '#c8c8d8';
    ctx.fillText(chain.short[0], 7, yt + 27);
    if (chain.short[1]) ctx.fillText(chain.short[1], 7, yt + 39);

    // ── SNR badge ────────────────────────────────────────────
    const q = channelQuality[chIdx];
    if (q && q.snr_db !== undefined) {
        const snr = q.snr_db;
        let dotColor;
        if (snr >= SNR_GOOD)      dotColor = '#22c55e';
        else if (snr >= SNR_WARN) dotColor = '#f59e0b';
        else                      dotColor = '#ef4444';

        // Dot
        ctx.beginPath();
        ctx.arc(10, yt + 52, 4, 0, Math.PI * 2);
        ctx.fillStyle = dotColor;
        ctx.fill();

        // SNR text
        ctx.font = '9px "Inter", system-ui, sans-serif';
        ctx.fillStyle = dotColor;
        ctx.fillText(snr.toFixed(1) + 'dB', 18, yt + 56);
    }

    // ── Emission-delay bar ───────────────────────────────────
    ctx.fillStyle = '#08080c';
    ctx.fillRect(sx, yb, scopeWidth - sx, H_LEGEND);

    if (msPerBin === 0 || !chain.stations || chain.stations.length === 0) {
        // Still apply dim overlay even if no station data
        if (heardOnly() && !isChannelHeard(chIdx)) {
            ctx.fillStyle = '#08080c';
            ctx.globalAlpha = 0.72;
            ctx.fillRect(0, yt, scopeWidth, ROW_HEIGHT);
            ctx.globalAlpha = 1.0;
        }
        return;
    }

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

    // ── "Heard only" dim overlay ─────────────────────────────
    // When the filter is active and this channel is below SNR threshold,
    // overlay a translucent dark rectangle over the entire row.
    if (heardOnly() && !isChannelHeard(chIdx)) {
        ctx.fillStyle = '#08080c';
        ctx.globalAlpha = 0.72;
        ctx.fillRect(0, yt, scopeWidth, ROW_HEIGHT);
        ctx.globalAlpha = 1.0;
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
