// tdoa_panel.js — TDOA / signal-quality table for ubersdr_loran
//
// Populates #tdoa-tbody with one row per active TDOA measurement pair.
// Data sources:
//   • GET /api/tdoa    — polled at 1 Hz, returns TDOAMeasurement[]
//   • GET /api/quality — polled at 1 Hz, returns {channels: ChannelQuality[], wall_clock_ms}
//
// TDOAMeasurement JSON fields (from tdoa.go):
//   chain_gri, chain_name, master_id, secondary_id,
//   emission_us, measured_us, tdoa_us, peak_bin, sub_bin, snr_db, valid, updated_at
//
// ChannelQuality JSON fields (from decoder.go):
//   ch_idx, gri, peak_bin, peak_pwr, noise_pwr, snr_db, navgs
//
// Depends on:
//   • window.BASE_PATH
//   • window.loranChains  (set by loran_c.js after bootstrap)
//   • window.loranTraceColors
//   • window.loranWsSubscribe(fn)

'use strict';

(function () {

const BASE_PATH = (typeof window.BASE_PATH === 'string') ? window.BASE_PATH : '';

// Poll interval (ms).
// Kept at 5 s to stay within the UberSDR reverse-proxy rate limit.
const POLL_MS = 5000;

// SNR thresholds (dB)
const SNR_GOOD = 15;
const SNR_WARN  = 8;

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// Latest quality map: key = ch_idx, value = ChannelQuality object
const qualityMap = {};

// Latest TDOA array (TDOAMeasurement[])
let latestTDOA = [];

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function snrDotClass(snr) {
    if (snr === undefined || snr === null || isNaN(snr)) return 'snr-none';
    if (snr >= SNR_GOOD) return 'snr-good';
    if (snr >= SNR_WARN)  return 'snr-warn';
    return 'snr-bad';
}

function snrValClass(snr) {
    if (snr === undefined || snr === null || isNaN(snr)) return 'val-muted';
    if (snr >= SNR_GOOD) return 'val-good';
    if (snr >= SNR_WARN)  return 'val-warn';
    return 'val-bad';
}

function fmtSNR(snr) {
    if (snr === undefined || snr === null || isNaN(snr)) return '—';
    return snr.toFixed(1) + ' dB';
}

function fmtTDOA(us) {
    if (us === undefined || us === null || isNaN(us)) return '—';
    const sign = us >= 0 ? '+' : '';
    return sign + us.toFixed(1) + ' µs';
}

// Look up chain display name from window.loranChains by GRI
function chainDisplayName(gri) {
    const chains = window.loranChains;
    if (!chains) return 'GRI ' + gri;
    const c = chains.find(ch => ch.gri === gri);
    if (!c) return 'GRI ' + gri;
    if (c.short && c.short.length > 0) return c.short[0];
    return c.name || ('GRI ' + gri);
}

// Look up trace colour for a chain by GRI
function chainColor(gri) {
    const chains = window.loranChains;
    const colors = window.loranTraceColors;
    if (!chains || !colors) return '#888';
    const idx = chains.findIndex(c => c.gri === gri);
    if (idx < 0) return '#888';
    return colors[idx % colors.length];
}

// Find the ch_idx for a given GRI (matches how TDOAEngine iterates ChainDB)
function griToChIdx(gri) {
    const chains = window.loranChains;
    if (!chains) return -1;
    return chains.findIndex(c => c.gri === gri);
}

// ---------------------------------------------------------------------------
// Render table
// ---------------------------------------------------------------------------

function renderTable(measurements) {
    const tbody = document.getElementById('tdoa-tbody');
    if (!tbody) return;

    if (!measurements || measurements.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" class="val-muted" ' +
            'style="padding:12px 10px;text-align:center">No measurements yet…</td></tr>';
        return;
    }

    // Sort by GRI then secondary_id
    const sorted = measurements.slice().sort((a, b) => {
        if (a.chain_gri !== b.chain_gri) return a.chain_gri - b.chain_gri;
        return (a.secondary_id || '').localeCompare(b.secondary_id || '');
    });

    let html = '';
    sorted.forEach(m => {
        const gri   = m.chain_gri;
        const color = chainColor(gri);

        // Master SNR comes from quality map keyed by ch_idx
        const chIdx    = griToChIdx(gri);
        const q        = chIdx >= 0 ? qualityMap[chIdx] : null;
        const masterSNR = q ? q.snr_db : null;

        // Secondary SNR comes directly from the TDOA measurement
        const secSNR = m.snr_db;

        const mDot = `<span class="snr-dot ${snrDotClass(masterSNR)}"></span>`;
        const sDot = `<span class="snr-dot ${snrDotClass(secSNR)}"></span>`;

        const validCell = m.valid
            ? '<span style="color:var(--ok)">✓</span>'
            : '<span style="color:var(--err)">✗</span>';

        // Accent bar + chain name
        const nameCell =
            `<span style="display:inline-block;width:3px;height:12px;` +
            `background:${color};border-radius:2px;margin-right:6px;vertical-align:middle"></span>` +
            `<span style="color:${color}">${escHtml(chainDisplayName(gri))}</span>`;

        html += `<tr>` +
            `<td>${nameCell}</td>` +
            `<td>${mDot}<span class="${snrValClass(masterSNR)}">${fmtSNR(masterSNR)}</span></td>` +
            `<td>${escHtml(m.secondary_id || '?')}</td>` +
            `<td>${sDot}<span class="${snrValClass(secSNR)}">${fmtSNR(secSNR)}</span></td>` +
            `<td class="val-muted">${fmtTDOA(m.tdoa_us)}</td>` +
            `<td>${validCell}</td>` +
            `</tr>`;
    });

    tbody.innerHTML = html;
}

function escHtml(s) {
    return String(s)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;');
}

// ---------------------------------------------------------------------------
// Poll REST endpoints
// ---------------------------------------------------------------------------

async function poll() {
    try {
        const [tdoaResp, qualResp] = await Promise.all([
            fetch(BASE_PATH + '/api/tdoa'),
            fetch(BASE_PATH + '/api/quality'),
        ]);

        if (tdoaResp.ok) {
            const data = await tdoaResp.json();
            // /api/tdoa returns a TDOAMeasurement[] directly
            latestTDOA = Array.isArray(data) ? data : [];
        }

        if (qualResp.ok) {
            const data = await qualResp.json();
            // /api/quality returns {channels: ChannelQuality[], wall_clock_ms: ...}
            const channels = data.channels || data; // handle both shapes
            if (Array.isArray(channels)) {
                channels.forEach(q => {
                    if (q.ch_idx !== undefined) qualityMap[q.ch_idx] = q;
                });
            }
        }

        renderTable(latestTDOA);
    } catch (e) {
        // Network error — silently ignore, will retry
    }
}

// ---------------------------------------------------------------------------
// WebSocket message handler (future: server may push these)
// ---------------------------------------------------------------------------

function onWsMessage(msg) {
    switch (msg.type) {
        case 'tdoa_update':
            if (Array.isArray(msg.measurements)) {
                latestTDOA = msg.measurements;
                renderTable(latestTDOA);
            }
            break;
        case 'quality_update': {
            const channels = msg.channels || msg.quality;
            if (Array.isArray(channels)) {
                channels.forEach(q => {
                    if (q.ch_idx !== undefined) qualityMap[q.ch_idx] = q;
                });
                renderTable(latestTDOA);
            }
            break;
        }
    }
}

// ---------------------------------------------------------------------------
// Init — called by loran_c.js after bootstrap
// ---------------------------------------------------------------------------

window.tdoaPanelInit = function () {
    // Subscribe to WS messages
    if (typeof window.loranWsSubscribe === 'function') {
        window.loranWsSubscribe(onWsMessage);
    } else {
        setTimeout(() => {
            if (typeof window.loranWsSubscribe === 'function') {
                window.loranWsSubscribe(onWsMessage);
            }
        }, 2000);
    }

    // Start polling
    poll();
    setInterval(poll, POLL_MS);
};

})();
