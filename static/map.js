// map.js — Leaflet map panel for ubersdr_loran
//
// Displays:
//   • Transmitter CircleMarkers (loaded from /api/chains at bootstrap)
//   • Receiver CircleMarker (from receiver_pos WS message)
//   • LOP polylines (from lop_update WS push)
//   • Position fix marker (from fix_update WS push)
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

// GRIs that have at least one valid TDOA measurement (i.e. we can hear them)
const heardGRIs = new Set();

// Latest LOP array — kept so we can re-apply filter on checkbox toggle
let latestLops = null;

// SNR per station: key = "GRI_stationId", value = snr_db (number or null)
const txSNR = {};

// Station metadata: key = "GRI_stationId", value = {id, name}
const txMeta = {};

// ---------------------------------------------------------------------------
// Initialise map
// ---------------------------------------------------------------------------

function heardOnly() {
    const cb = document.getElementById('valid-only');
    return cb ? cb.checked : false;
}

// Fit the map view to all currently-visible (non-dimmed) transmitter markers.
// Called after any filter change so the map always shows the relevant stations.
// If no markers are visible, does nothing.
function fitVisibleMarkers() {
    if (!map) return;
    const latlngs = [];
    Object.entries(txMarkers).forEach(([key, marker]) => {
        // A marker is "visible" if its opacity is not the ghost value (0.12)
        const opts = marker.options;
        if (opts.opacity > 0.12) {
            latlngs.push(marker.getLatLng());
        }
    });
    // Also include receiver marker if present
    if (window._rxMarkerLatLng) latlngs.push(window._rxMarkerLatLng);
    if (latlngs.length === 0) return;
    if (latlngs.length === 1) {
        map.setView(latlngs[0], Math.max(map.getZoom(), 5));
        return;
    }
    map.fitBounds(L.latLngBounds(latlngs), { padding: [40, 40], maxZoom: 8 });
}

// Apply/remove the "heard only" and search filters to transmitter markers,
// then fit the map to the visible markers.
// A chain is "heard" if heardGRIs contains its GRI.
function applyHeardFilter() {
    const filterHeard  = heardOnly();
    const chains = window.loranChains;
    if (!chains) return;

    chains.forEach((chain, chIdx) => {
        const heard = heardGRIs.has(chain.gri);
        // Search filter: use loran_c.js helper if available
        const matchesSearch = (typeof window.loranIsChainMatch === 'function')
            ? window.loranIsChainMatch(chIdx)
            : true;
        const dim = (filterHeard && !heard) || !matchesSearch;

        if (!chain.stations) return;
        chain.stations.forEach((st, stIdx) => {
            const key = chain.gri + '_' + st.id;
            const marker = txMarkers[key];
            if (!marker) return;
            if (dim) {
                marker.setStyle({ opacity: 0.12, fillOpacity: 0.06 });
                // Hide the permanent tooltip so it doesn't float over a ghost marker
                marker.closeTooltip();
            } else {
                const isMaster = stIdx === 0;
                marker.setStyle({
                    opacity:     1,
                    fillOpacity: isMaster ? 0.9 : 0.55,
                });
                // Restore the permanent tooltip
                marker.openTooltip();
            }
        });
    });

    // Re-apply LOP filter with cached data
    if (latestLops !== null) {
        updateLOPs(latestLops, window.loranChains, window.loranTraceColors);
    }

    // Fit map to visible markers after filter change
    fitVisibleMarkers();
}

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

    // Wire "heard only" checkbox
    const cb = document.getElementById('valid-only');
    if (cb) cb.addEventListener('change', applyHeardFilter);

    // Wire search filter — re-apply marker visibility on every keystroke
    if (typeof window.loranSearchSubscribe === 'function') {
        window.loranSearchSubscribe(() => applyHeardFilter());
    }

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

// Build the permanent label text for a transmitter marker.
// Shows: "ID Name  X.X dB" (SNR omitted if not yet known)
function buildTxLabel(id, name, snr) {
    const snrStr = (snr !== null && snr !== undefined && !isNaN(snr))
        ? `  <span style="opacity:0.7">${snr.toFixed(1)} dB</span>`
        : '';
    return `<b>${id}</b> ${name}${snrStr}`;
}

// Build the click popup content for a transmitter marker.
function buildTxPopup(meta, snr) {
    const role = meta.isMaster ? '📡 Master' : '📡 Secondary';
    const hasSnr = snr !== null && snr !== undefined && !isNaN(snr);
    const snrStr = hasSnr ? `${snr.toFixed(1)} dB` : '—';
    const snrColor = !hasSnr ? '#888'
        : snr >= 15 ? '#22c55e'
        : snr >= 8  ? '#f59e0b'
        : '#ef4444';
    return `<b>${role}: ${meta.id}</b><br>` +
           `${meta.name}<br>` +
           `<span style="color:#888">GRI ${meta.gri} — ${meta.chainName}</span><br>` +
           `${meta.lat.toFixed(4)}°, ${meta.lon.toFixed(4)}°<br>` +
           `SNR: <b style="color:${snrColor}">${snrStr}</b>`;
}

// Update the permanent label and popup for a single marker key with latest SNR.
function updateTxTooltip(key) {
    const marker = txMarkers[key];
    if (!marker) return;
    const meta = txMeta[key];
    if (!meta) return;
    const snr = txSNR[key] !== undefined ? txSNR[key] : null;
    marker.setTooltipContent(buildTxLabel(meta.id, meta.name, snr));
    if (marker.getPopup()) {
        marker.setPopupContent(buildTxPopup(meta, snr));
    }
}

function placeTransmitterMarkers(chains, traceColors) {
    if (!map) return;
    chains.forEach((chain, chIdx) => {
        // Use the same trace colour as the scope row for this chain
        const color = traceColors[chIdx % traceColors.length];
        if (!chain.stations) return;
        chain.stations.forEach((st, stIdx) => {
            if (!st.lat || !st.lon || (st.lat === 0 && st.lon === 0)) return;
            const key = chain.gri + '_' + st.id;

            const isMaster = stIdx === 0;

            // Store full metadata for label/popup updates
            txMeta[key] = {
                id:        st.id,
                name:      st.name,
                gri:       chain.gri,
                chainName: chain.name || ('GRI ' + chain.gri),
                lat:       st.lat,
                lon:       st.lon,
                isMaster:  isMaster,
            };

            if (txMarkers[key]) return; // already placed

            const marker = L.circleMarker([st.lat, st.lon], {
                radius:      isMaster ? 7 : 5,
                color:       color,
                fillColor:   color,
                fillOpacity: isMaster ? 0.9 : 0.55,
                weight:      isMaster ? 2.5 : 1.5,
                opacity:     1,
            }).addTo(map);

            // Permanent label to the right of the marker
            marker.bindTooltip(
                buildTxLabel(st.id, st.name, null),
                { direction: 'right', offset: [6, 0], permanent: true, className: 'tx-label' }
            );

            // Click popup with full details
            marker.bindPopup(buildTxPopup(txMeta[key], null), { maxWidth: 220 });

            txMarkers[key] = marker;
        });
    });

    // Fit map to all transmitter markers on initial placement
    fitVisibleMarkers();
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

    // Store for fitVisibleMarkers
    window._rxMarkerLatLng = L.latLng(lat, lon);

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

        // Fit to all visible markers (transmitters + receiver) on first placement
        fitVisibleMarkers();
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

    // Cache for re-application when checkbox toggles
    latestLops = lops;

    const filter = heardOnly();

    // Track which keys are still active
    const activeKeys = new Set();

    lops.forEach(lop => {
        if (!lop.points || lop.points.length < 2) return;
        if (!lop.valid) return;
        // When "heard only" is active, skip LOPs for chains we haven't heard
        if (filter && !heardGRIs.has(lop.chain_gri)) return;

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
        // Hide fix marker if invalid — circleMarker has no setOpacity(), use setStyle()
        if (fixMarker) {
            fixMarker.setStyle({ opacity: 0.3, fillOpacity: 0.1 });
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
        fixMarker.setStyle({ opacity: 1, fillOpacity: 0.9 });
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
    const residStr = fix.rms_km !== undefined
        ? (fix.rms_km < 1
            ? (fix.rms_km * 1000).toFixed(0) + ' m RMS'
            : fix.rms_km.toFixed(3) + ' km RMS')
        : '?';
    const errStr = errKm !== null && errKm !== undefined
        ? (errKm < 1
            ? (errKm * 1000).toFixed(0) + ' m'
            : errKm.toFixed(2) + ' km')
        : '?';
    const iterStr = fix.iterations !== undefined ? fix.iterations : '?';
    return `<b>🟢 Loran fix</b><br>` +
           `Lat: ${fix.lat.toFixed(5)}°<br>` +
           `Lon: ${fix.lon.toFixed(5)}°<br>` +
           `Error vs known: <b>${errStr}</b><br>` +
           `Residual: ${residStr}<br>` +
           `LOPs used: ${fix.lop_count !== undefined ? fix.lop_count : '?'}<br>` +
           `Iterations: ${iterStr}`;
}

// ---------------------------------------------------------------------------
// WebSocket message handler
// ---------------------------------------------------------------------------

function onWsMessage(msg) {
    switch (msg.type) {
        case 'receiver_pos':
            setReceiverPos(msg.lat, msg.lon);
            break;

        case 'quality_update': {
            // Update master station SNR labels.
            // quality_update channels are indexed by ch_idx which maps to chain index.
            const channels = msg.channels || msg.quality;
            const chains = window.loranChains;
            if (Array.isArray(channels) && chains) {
                channels.forEach(q => {
                    if (q.ch_idx === undefined || q.ch_idx >= chains.length) return;
                    const chain = chains[q.ch_idx];
                    if (!chain || !chain.stations || chain.stations.length === 0) return;
                    // Master is stations[0]
                    const masterId = chain.stations[0].id;
                    const key = chain.gri + '_' + masterId;
                    txSNR[key] = q.snr_db;
                    updateTxTooltip(key);
                });
            }
            break;
        }

        case 'tdoa_update':
            // Track which GRIs have at least one valid measurement (i.e. we can hear them)
            if (Array.isArray(msg.measurements)) {
                heardGRIs.clear();
                msg.measurements.forEach(m => {
                    if (m.valid) heardGRIs.add(m.chain_gri);
                    // Update secondary station SNR label
                    const key = m.chain_gri + '_' + m.secondary_id;
                    txSNR[key] = m.snr_db;
                    updateTxTooltip(key);
                });
                applyHeardFilter();
            }
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
