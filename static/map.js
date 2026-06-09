// map.js — Leaflet map panel for ubersdr_loran
//
// Displays:
//   • Transmitter CircleMarkers (loaded from /api/chains)
//   • Receiver CircleMarker (from receiver_pos WS message)
//   • LOP polylines (from /api/lops, polled 1 Hz)
//   • Position fix marker (from /api/fix, polled 1 Hz)
//
// Depends on:
//   • Leaflet.js (loaded before this script in index.html)
//   • window.BASE_PATH  (set by index.html)
//   • window.loranChains  (set by loran_c.js after bootstrap)
//   • window.loranTraceColors  (set by loran_c.js)
//   • window.loranWsSubscribe(fn)  (set by loran_c.js — calls fn(msg) for JSON WS frames)

'use strict';

(function () {

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const BASE_PATH = (typeof window.BASE_PATH === 'string') ? window.BASE_PATH : '';

// Tile layer — OpenStreetMap
const TILE_URL = 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png';
const TILE_ATTR = '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors';

// Station colour palette (matches loran_c.js STATION_COLORS)
const STATION_COLORS = ['#e74c3c', '#f1c40f', '#2ecc71', '#3498db', '#95a5a6'];

// Poll interval for REST endpoints (ms).
// Kept at 5 s to stay within the UberSDR reverse-proxy rate limit.
const POLL_MS = 5000;

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

let map = null;
let initialised = false;

// Transmitter markers: key = "GRI_stationId"
const txMarkers = {};

// Receiver marker (known position from /api/description)
let rxMarker   = null;
let rxLat      = 0;
let rxLon      = 0;

// LOP polylines: key = lopKey (GRI + "_" + secId)
const lopLines = {};

// Fix marker (Loran-computed position)
let fixMarker  = null;
let fixCircle  = null;
let firstFix   = true;

// Error line between known position and Loran fix
let errorLine  = null;

// ---------------------------------------------------------------------------
// Initialise map
// ---------------------------------------------------------------------------

function initMap() {
    if (initialised) return;
    initialised = true;

    map = L.map('map-panel', {
        center: [50, 0],
        zoom: 4,
        zoomControl: true,
        attributionControl: true,
    });

    L.tileLayer(TILE_URL, {
        attribution: TILE_ATTR,
        maxZoom: 18,
    }).addTo(map);

    // Fetch receiver position immediately (may also arrive via WS receiver_pos)
    fetchReceiverPos();

    // Start polling REST endpoints
    pollLOPs();
    setInterval(pollLOPs, POLL_MS);

    // Subscribe to WS JSON messages
    if (typeof window.loranWsSubscribe === 'function') {
        window.loranWsSubscribe(onWsMessage);
    } else {
        // loran_c.js may not have set it yet — retry after a short delay
        setTimeout(() => {
            if (typeof window.loranWsSubscribe === 'function') {
                window.loranWsSubscribe(onWsMessage);
            }
        }, 2000);
    }
}

// ---------------------------------------------------------------------------
// Place transmitter markers from chain data
// ---------------------------------------------------------------------------

function placeTransmitterMarkers(chains, traceColors) {
    if (!map) return;
    chains.forEach((chain, chIdx) => {
        const traceColor = traceColors[chIdx % traceColors.length];
        if (!chain.stations) return;
        chain.stations.forEach((st, stIdx) => {
            if (!st.lat || !st.lon || (st.lat === 0 && st.lon === 0)) return;
            const key = chain.gri + '_' + st.id;
            if (txMarkers[key]) return; // already placed

            const color = STATION_COLORS[stIdx % STATION_COLORS.length];
            const isMaster = stIdx === 0;

            const marker = L.circleMarker([st.lat, st.lon], {
                radius:      isMaster ? 7 : 5,
                color:       color,
                fillColor:   color,
                fillOpacity: isMaster ? 0.9 : 0.7,
                weight:      isMaster ? 2 : 1,
                opacity:     1,
            }).addTo(map);

            marker.bindTooltip(
                `<b>${st.id} ${st.name}</b><br>` +
                `GRI ${chain.gri} — ${chain.name}<br>` +
                `${st.lat.toFixed(4)}°, ${st.lon.toFixed(4)}°`,
                { direction: 'top', offset: [0, -6] }
            );

            txMarkers[key] = marker;
        });
    });
}

// ---------------------------------------------------------------------------
// Receiver marker
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Haversine distance (km) between two lat/lon points
// ---------------------------------------------------------------------------

function haversineKm(lat1, lon1, lat2, lon2) {
    const R  = 6371.0;
    const dL = (lat2 - lat1) * Math.PI / 180;
    const dl = (lon2 - lon1) * Math.PI / 180;
    const a  = Math.sin(dL/2)**2 +
               Math.cos(lat1 * Math.PI/180) * Math.cos(lat2 * Math.PI/180) *
               Math.sin(dl/2)**2;
    return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
}

// ---------------------------------------------------------------------------
// Receiver marker (known GPS position from /api/description)
// ---------------------------------------------------------------------------

async function fetchReceiverPos() {
    try {
        const resp = await fetch(BASE_PATH + '/api/receiver');
        if (!resp.ok) return;
        const data = await resp.json();
        if (data.lat && data.lon) setReceiverPos(data.lat, data.lon);
    } catch (e) { /* ignore */ }
}

function setReceiverPos(lat, lon) {
    if (!map) return;
    if (lat === 0 && lon === 0) return;

    rxLat = lat;
    rxLon = lon;

    if (rxMarker) {
        rxMarker.setLatLng([lat, lon]);
        rxMarker.setTooltipContent(buildRxTooltip(lat, lon));
    } else {
        rxMarker = L.circleMarker([lat, lon], {
            radius:      9,
            color:       '#00d4ff',
            fillColor:   '#00d4ff',
            fillOpacity: 0.25,
            weight:      2,
            opacity:     1,
        }).addTo(map);

        rxMarker.bindTooltip(buildRxTooltip(lat, lon),
            { direction: 'top', offset: [0, -8], permanent: false }
        );

        // Pan to receiver on first placement
        map.setView([lat, lon], 5, { animate: true });
    }

    // Redraw error line if fix is already known
    updateErrorLine();
}

function buildRxTooltip(lat, lon) {
    return `<b>📡 Known position</b><br>` +
           `${lat.toFixed(5)}°, ${lon.toFixed(5)}°<br>` +
           `<span style="color:#6b6b7a;font-size:10px">From /api/description (GPS)</span>`;
}

// ---------------------------------------------------------------------------
// Error line between known position and Loran fix
// ---------------------------------------------------------------------------

function updateErrorLine() {
    if (!map) return;
    if (!rxLat || !rxLon) { hideErrorLine(); return; }
    if (!fixMarker) { hideErrorLine(); return; }
    const fixLL = fixMarker.getLatLng();
    if (!fixLL) { hideErrorLine(); return; }

    const latLngs = [[rxLat, rxLon], [fixLL.lat, fixLL.lng]];

    if (errorLine) {
        errorLine.setLatLngs(latLngs);
        errorLine.setStyle({ opacity: 0.8 });
    } else {
        errorLine = L.polyline(latLngs, {
            color:     '#f59e0b',
            weight:    2,
            opacity:   0.8,
            dashArray: '5 4',
        }).addTo(map);
        errorLine.bindTooltip('Error vector (known → Loran fix)', { sticky: true });
    }
}

function hideErrorLine() {
    if (errorLine) errorLine.setStyle({ opacity: 0 });
}

// ---------------------------------------------------------------------------
// LOP polylines
// ---------------------------------------------------------------------------

function updateLOPs(lops, chains, traceColors) {
    if (!map || !lops) return;

    // Track which keys are still active
    const activeKeys = new Set();

    lops.forEach(lop => {
        if (!lop.points || lop.points.length < 2) return;
        if (!lop.valid) return;

        // LOP JSON fields: chain_gri, secondary_id, tdoa_us, points
        const gri = lop.chain_gri;
        const key = gri + '_' + lop.secondary_id;
        activeKeys.add(key);

        // Find chain index for colour
        const chIdx = chains ? chains.findIndex(c => c.gri === gri) : -1;
        const color = (chIdx >= 0 && traceColors)
            ? traceColors[chIdx % traceColors.length]
            : '#888888';

        const latLngs = lop.points.map(p => [p.lat, p.lon]);

        if (lopLines[key]) {
            lopLines[key].setLatLngs(latLngs);
        } else {
            lopLines[key] = L.polyline(latLngs, {
                color:   color,
                weight:  2,
                opacity: 0.7,
                dashArray: '6 4',
            }).addTo(map);

            lopLines[key].bindTooltip(
                `LOP GRI ${gri} — ${lop.secondary_id}<br>` +
                `TDOA ${lop.tdoa_us !== undefined ? lop.tdoa_us.toFixed(1) + ' µs' : '?'}`,
                { sticky: true }
            );
        }
    });

    // Remove stale LOPs
    Object.keys(lopLines).forEach(key => {
        if (!activeKeys.has(key)) {
            map.removeLayer(lopLines[key]);
            delete lopLines[key];
        }
    });
}

// ---------------------------------------------------------------------------
// Position fix marker
// ---------------------------------------------------------------------------

function updateFix(fix) {
    if (!map || !fix || !fix.valid) {
        // Hide fix marker if invalid
        if (fixMarker) {
            fixMarker.setOpacity(0.3);
            if (fixCircle) fixCircle.setStyle({ opacity: 0.1, fillOpacity: 0.05 });
        }
        hideErrorLine();
        document.getElementById('fix-badge').textContent = 'No fix';
        document.getElementById('fix-badge').style.color = '';
        return;
    }

    const lat = fix.lat;
    const lon = fix.lon;

    // Compute error distance from known receiver position
    const errKm = (rxLat && rxLon)
        ? haversineKm(rxLat, rxLon, lat, lon)
        : null;

    if (fixMarker) {
        fixMarker.setLatLng([lat, lon]);
        fixMarker.setOpacity(1);
        if (fixCircle) {
            fixCircle.setLatLng([lat, lon]);
            fixCircle.setStyle({ opacity: 0.6, fillOpacity: 0.15 });
        }
    } else {
        // Accuracy circle (~5 km radius)
        fixCircle = L.circle([lat, lon], {
            radius:      5000,
            color:       '#22c55e',
            fillColor:   '#22c55e',
            fillOpacity: 0.12,
            weight:      1,
            opacity:     0.5,
            dashArray:   '4 4',
        }).addTo(map);

        fixMarker = L.circleMarker([lat, lon], {
            radius:      8,
            color:       '#22c55e',
            fillColor:   '#22c55e',
            fillOpacity: 0.9,
            weight:      2,
            opacity:     1,
        }).addTo(map);

        fixMarker.bindPopup(buildFixPopup(fix, errKm), { maxWidth: 240 });
    }

    // Update popup content
    if (fixMarker.isPopupOpen()) {
        fixMarker.getPopup().setContent(buildFixPopup(fix, errKm));
    }

    // Update badge — show error distance if known position available
    const badge = document.getElementById('fix-badge');
    if (errKm !== null) {
        const errStr = errKm < 1
            ? (errKm * 1000).toFixed(0) + ' m error'
            : errKm.toFixed(1) + ' km error';
        badge.textContent = errStr;
        badge.title = `Loran fix: ${lat.toFixed(4)}°, ${lon.toFixed(4)}°`;
    } else {
        badge.textContent = `${lat.toFixed(4)}°, ${lon.toFixed(4)}°`;
    }
    badge.style.color = '#22c55e';

    // Draw error line between known position and Loran fix
    updateErrorLine();

    // Auto-zoom on first valid fix
    if (firstFix) {
        firstFix = false;
        map.setView([lat, lon], 6, { animate: true });
    }
}

function buildFixPopup(fix, errKm) {
    const residStr = fix.residual_us !== undefined
        ? fix.residual_us.toFixed(1) + ' µs'
        : '?';
    const hdopStr = fix.hdop !== undefined
        ? fix.hdop.toFixed(2)
        : '?';
    const errStr = errKm !== null && errKm !== undefined
        ? (errKm < 1
            ? (errKm * 1000).toFixed(0) + ' m'
            : errKm.toFixed(2) + ' km')
        : '?';
    return `<b>🟢 Loran fix</b><br>` +
           `Lat: ${fix.lat.toFixed(5)}°<br>` +
           `Lon: ${fix.lon.toFixed(5)}°<br>` +
           `Error vs known: <b>${errStr}</b><br>` +
           `Residual: ${residStr}<br>` +
           `HDOP: ${hdopStr}<br>` +
           `LOPs used: ${fix.n_lops || '?'}`;
}

// ---------------------------------------------------------------------------
// Poll REST endpoints
// ---------------------------------------------------------------------------

async function pollLOPs() {
    if (!map) return;

    const chains     = window.loranChains;
    const traceColors = window.loranTraceColors;

    try {
        const [lopsResp, fixResp] = await Promise.all([
            fetch(BASE_PATH + '/api/lops'),
            fetch(BASE_PATH + '/api/fix'),
        ]);

        if (lopsResp.ok) {
            const lops = await lopsResp.json();
            updateLOPs(lops, chains, traceColors);
        }

        if (fixResp.ok) {
            const fix = await fixResp.json();
            updateFix(fix);
        }
    } catch (e) {
        // Network error — silently ignore, will retry
    }
}

// ---------------------------------------------------------------------------
// WebSocket message handler
// ---------------------------------------------------------------------------

function onWsMessage(msg) {
    switch (msg.type) {
        case 'receiver_pos':
            setReceiverPos(msg.lat, msg.lon);
            break;
        case 'lop_update':
            if (msg.lops) {
                updateLOPs(msg.lops, window.loranChains, window.loranTraceColors);
            }
            break;
        case 'fix_update':
            if (msg.fix) updateFix(msg.fix);
            break;
    }
}

// ---------------------------------------------------------------------------
// Public API — called by loran_c.js after bootstrap completes
// ---------------------------------------------------------------------------

window.mapInit = function (chains, traceColors) {
    window.loranChains      = chains;
    window.loranTraceColors = traceColors;
    initMap();
    placeTransmitterMarkers(chains, traceColors);
};

// Also expose for late-arriving receiver_pos
window.mapSetReceiverPos = setReceiverPos;

})();
