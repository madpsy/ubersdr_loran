// loran_c.js — Loran-C scope renderer for ubersdr_loran
//
// Ported from KiwiSDR web/extensions/Loran_C/Loran_C.js
// Original copyright (c) 2016-2023 John Seamons, ZL4VO/KF6VO
// Port copyright (c) 2026 UberSDR project
//
// Communicates with server.go via a plain WebSocket (no KiwiSDR ext_* APIs).
// Binary frames:  "DAT" + cmd(1) + ch(1) + scope_bytes(nbucket)
// Text frames:    JSON { type, ... }
// Control frames: JSON sent to server

'use strict';

// ---------------------------------------------------------------------------
// GRI chain table (mirrors gri_s / gri_2s / emission_delay in Loran_C.js)
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

// Emission (tx) delay data from Markus Vester, DF6NM
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

const CMD_SCOPE_DATA  = 0;
const CMD_SCOPE_RESET = 1;

const SCOPE_START_X = 125;  // pixels reserved for left legend
const H_LEGEND      = 20;   // pixels for bottom legend bar

// Default chains (indices into GRI_LIST)
const DEFAULT_CHAIN = [7, 3]; // Western Russia, China BPL

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

let ws = null;
let msPerBin = 0;
let canvas, ctx;
let scopeWidth = 0, scopeHeight = 0;

// Per-channel state
const ch = [
    { gri: 0, griSel: -1, gain: 0, avgAlgo: 1, avgParam: 50, nbuckets: 0 },
    { gri: 0, griSel: -1, gain: 0, avgAlgo: 1, avgParam: 50, nbuckets: 0 },
];

// ---------------------------------------------------------------------------
// WebSocket connection
// ---------------------------------------------------------------------------

function connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const wsUrl = `${proto}://${location.host}/ws`;
    ws = new WebSocket(wsUrl);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
        console.log('WS connected');
        document.getElementById('status').textContent = 'Connected';
        document.getElementById('status').className = 'status-ok';
        // Send start and current GRI settings
        sendControl({ type: 'start' });
        for (let i = 0; i < 2; i++) {
            if (ch[i].gri > 0) applyGRI(i, ch[i].gri);
        }
    };

    ws.onclose = () => {
        console.log('WS closed — reconnecting in 3s');
        document.getElementById('status').textContent = 'Disconnected — reconnecting…';
        document.getElementById('status').className = 'status-err';
        setTimeout(connect, 3000);
    };

    ws.onerror = (e) => {
        console.error('WS error', e);
    };

    ws.onmessage = (evt) => {
        if (evt.data instanceof ArrayBuffer) {
            handleBinary(evt.data);
        } else {
            handleText(evt.data);
        }
    };
}

function sendControl(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(obj));
    }
}

// ---------------------------------------------------------------------------
// Binary frame handler
// Matches server.go BroadcastScope wire format:
//   "DAT" (3 bytes) + cmd (1 byte) + payload (ch byte + nbucket bytes)
// ---------------------------------------------------------------------------

function handleBinary(buf) {
    const ba = new Uint8Array(buf);
    // Check "DAT" header
    if (ba[0] !== 0x44 || ba[1] !== 0x41 || ba[2] !== 0x54) return;

    const cmd     = ba[3];
    const chIdx   = ba[4];
    const data    = ba.slice(5);  // nbucket scope bytes

    if (chIdx < 0 || chIdx >= 2) return;
    ch[chIdx].nbuckets = data.length;

    drawScope(chIdx, cmd, data);
}

// ---------------------------------------------------------------------------
// Text frame handler
// ---------------------------------------------------------------------------

function handleText(raw) {
    let msg;
    try { msg = JSON.parse(raw); } catch(e) { return; }

    if (msg.type === 'ms_per_bin') {
        msPerBin = parseFloat(msg.ms_per_bin);
        console.log('ms_per_bin =', msPerBin);
        // Redraw legends with updated scale
        for (let i = 0; i < 2; i++) {
            if (ch[i].gri > 0) drawLegend(i, ch[i].gri);
        }
    }
}

// ---------------------------------------------------------------------------
// Canvas drawing — mirrors loran_c_recv() and loran_c_draw_legend() in Loran_C.js
// ---------------------------------------------------------------------------

function drawScope(chIdx, cmd, data) {
    if (!canvas) return;
    const w  = scopeWidth;
    const h  = scopeHeight;
    const sx = SCOPE_START_X;
    const sy = (chIdx + 1) * h / 2 - H_LEGEND;
    const yh = h / 2 - H_LEGEND;

    if (cmd === CMD_SCOPE_RESET) {
        ctx.fillStyle = 'black';
        ctx.fillRect(sx, chIdx * h / 2, w - sx, yh);
        drawLegend(chIdx, ch[chIdx].gri);
    }

    const color = chIdx === 0 ? 'cyan' : 'violet';
    const blen = data.length;

    for (let i = 0; i < blen; i++) {
        const z = data[i] / 255;
        ctx.fillStyle = 'black';
        ctx.fillRect(sx + i, sy, 1, -yh);
        ctx.fillStyle = color;
        ctx.fillRect(sx + i, sy, 1, z * -yh);
    }
    // Clear right of data
    ctx.fillStyle = 'black';
    ctx.fillRect(sx + blen, sy, w - sx - blen + 1, -yh);
}

function drawLegend(chIdx, gri) {
    if (!canvas || msPerBin === 0 || gri === 0) return;

    const w       = scopeWidth;
    const h       = scopeHeight;
    const sx      = SCOPE_START_X;
    const sy      = (chIdx + 1) * h / 2;
    const yh      = h / 2 - H_LEGEND;
    const hbar    = 6;
    const off     = 8;

    ctx.font = '12px Verdana';

    // GRI label (top-left of channel area)
    ctx.fillStyle = 'white';
    ctx.fillText('GRI ' + gri, 0, sy - yh / 3 - H_LEGEND - off);

    // Chain name
    const entry = GRI_LIST.find(e => e.gri === gri);
    if (entry) {
        ctx.fillText(entry.short[0], 0, sy - yh / 3 - H_LEGEND + off);
        ctx.fillText(entry.short[1], 0, sy - yh / 3 - H_LEGEND + 3 * off);
    }

    // Emission delay bars
    const ed = EMISSION_DELAY[gri];
    if (!ed) return;

    for (let i = 0; i < ed.length; i++) {
        let ed0 = ed[i].d / 1000;
        let ed1 = (i < ed.length - 1) ? ed[i + 1].d / 1000 : gri / 100;

        ed0 = Math.round(ed0 / msPerBin);
        ed1 = Math.round(ed1 / msPerBin);

        ctx.fillStyle = STATION_COLORS[i % STATION_COLORS.length];
        ctx.fillRect(sx + ed0, sy - H_LEGEND, ed1 - ed0, hbar);
        ctx.fillStyle = 'white';
        ctx.fillText(ed[i].s, sx + ed0, sy - 3);
    }
}

// ---------------------------------------------------------------------------
// GRI control helpers
// ---------------------------------------------------------------------------

function applyGRI(chIdx, gri) {
    gri = parseInt(gri);
    if (isNaN(gri) || gri <= 0) return;

    // Hack: 5991 (US west coast eLoran test) uses 5990 on the server side
    const serverGRI = (gri === 5991) ? 5990 : gri;

    ch[chIdx].gri = gri;

    // Clear channel area and redraw legend
    if (canvas) {
        const h = scopeHeight;
        ctx.fillStyle = 'black';
        ctx.fillRect(0, chIdx * h / 2, scopeWidth, h / 2);
        ctx.font = '12px Verdana';
        drawLegend(chIdx, gri);
    }

    sendControl({ type: 'set_gri', ch: chIdx, gri: serverGRI });
}

// ---------------------------------------------------------------------------
// UI event handlers
// ---------------------------------------------------------------------------

function onGRIInput(chIdx) {
    const input = document.getElementById('gri-input-' + chIdx);
    const gri = parseInt(input.value);
    if (!isNaN(gri) && gri > 0) {
        // Sync dropdown
        const sel = document.getElementById('gri-select-' + chIdx);
        const idx = GRI_LIST.findIndex(e => e.gri === gri);
        sel.value = idx >= 0 ? idx : -1;
        applyGRI(chIdx, gri);
    }
}

function onGRISelect(chIdx) {
    const sel = document.getElementById('gri-select-' + chIdx);
    const idx = parseInt(sel.value);
    if (idx < 0 || idx >= GRI_LIST.length) return;
    const gri = GRI_LIST[idx].gri;
    document.getElementById('gri-input-' + chIdx).value = gri;
    applyGRI(chIdx, gri);
}

function onGainChange(chIdx) {
    const slider = document.getElementById('gain-' + chIdx);
    const val = parseInt(slider.value);
    ch[chIdx].gain = val;
    const label = document.getElementById('gain-label-' + chIdx);
    label.textContent = val === 0 ? 'Gain (auto-scale)' : 'Gain ' + val + ' dB';
    sendControl({ type: 'set_gain', ch: chIdx, gain: val });
}

function onAvgAlgoChange(chIdx) {
    const sel = document.getElementById('avg-algo-' + chIdx);
    const algo = parseInt(sel.value);
    ch[chIdx].avgAlgo = algo;
    sendControl({ type: 'set_avg_algo', ch: chIdx, algo: algo });
    // Re-send current param for this algo
    onAvgParamChange(chIdx);
}

function onAvgParamChange(chIdx) {
    const slider = document.getElementById('avg-param-' + chIdx);
    const sliderVal = parseInt(slider.value);
    ch[chIdx].avgParam = sliderVal;

    // Convert slider 0-100 to actual param value (mirrors loran_c_param_val in Loran_C.js)
    const algo = ch[chIdx].avgAlgo;
    const maxVals = [32, 512, 1.0];
    const minVals = [1, 1, 0];
    let param = sliderVal * maxVals[algo] / 100;
    if (maxVals[algo] > 1) param = Math.ceil(param);
    if (param < minVals[algo]) param = minVals[algo];

    const paramNames = ['Averages', 'Decay', 'Exp'];
    document.getElementById('avg-param-label-' + chIdx).textContent =
        paramNames[algo] + ' ' + param;

    sendControl({ type: 'set_avg_param', ch: chIdx, param: param });
}

// Canvas click — shift-click to align master station (mirrors loran_c_mousedown)
function onCanvasClick(evt) {
    if (!canvas) return;
    const rect = canvas.getBoundingClientRect();
    const x = evt.clientX - rect.left;
    const y = evt.clientY - rect.top;
    const chIdx = (y < scopeHeight / 2) ? 0 : 1;
    const offset = Math.round(x) - SCOPE_START_X;
    if (offset < 0 || offset >= ch[chIdx].nbuckets) return;
    sendControl({ type: 'set_offset', ch: chIdx, offset: offset });
}

// ---------------------------------------------------------------------------
// Resize handling
// ---------------------------------------------------------------------------

function resizeCanvas() {
    if (!canvas) return;
    const container = document.getElementById('scope-container');
    const w = container.clientWidth;
    scopeWidth  = w;
    scopeHeight = canvas.height; // fixed at 200px
    canvas.width = w;

    // Redraw legends after resize
    for (let i = 0; i < 2; i++) {
        if (ch[i].gri > 0) {
            ctx.fillStyle = 'black';
            ctx.fillRect(0, i * scopeHeight / 2, scopeWidth, scopeHeight / 2);
            ctx.font = '12px Verdana';
            drawLegend(i, ch[i].gri);
        }
    }
}

// ---------------------------------------------------------------------------
// Initialisation
// ---------------------------------------------------------------------------

function init() {
    canvas = document.getElementById('scope');
    ctx    = canvas.getContext('2d');

    // Populate GRI dropdowns
    for (let chIdx = 0; chIdx < 2; chIdx++) {
        const sel = document.getElementById('gri-select-' + chIdx);
        GRI_LIST.forEach((entry, idx) => {
            const opt = document.createElement('option');
            opt.value = idx;
            opt.textContent = entry.gri + ' ' + entry.name;
            sel.appendChild(opt);
        });

        // Set defaults
        const defaultIdx = DEFAULT_CHAIN[chIdx];
        sel.value = defaultIdx;
        const defaultGRI = GRI_LIST[defaultIdx].gri;
        document.getElementById('gri-input-' + chIdx).value = defaultGRI;
        ch[chIdx].gri = defaultGRI;

        // Initialise param labels
        onAvgParamChange(chIdx);
    }

    resizeCanvas();
    window.addEventListener('resize', resizeCanvas);
    canvas.addEventListener('click', onCanvasClick);

    connect();
}

document.addEventListener('DOMContentLoaded', init);
