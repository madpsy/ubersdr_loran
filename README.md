# ubersdr_loran

**Loran-C pulse-envelope viewer addon for [UberSDR](https://github.com/madpsy/ka9q_ubersdr)**

Connects to an UberSDR instance, requests a raw IQ stream centred on the **100 kHz Loran-C carrier**, decodes the pulse envelope for up to **two simultaneous GRI chains**, and serves a browser-based scope display.

---

## Screenshot

```
┌─────────────────────────────────────────────────────────────────┐
│  GRI 8000                                                       │
│  Western Russia                                                 │
│  (Chayka)                                                       │
│  ████████████████████████████████████████████████████████████  │
│  M Bryansk  W Petrozavodsk  X Slonim  Y Simferopol  Z Syzran   │
├─────────────────────────────────────────────────────────────────┤
│  GRI 6000                                                       │
│  China BPL                                                      │
│  Pucheng                                                        │
│  ████████████████████████████████████████████████████████████  │
│  M Pucheng                                                      │
└─────────────────────────────────────────────────────────────────┘
```

---

## How it works

```
UberSDR /ws
mode=iq, freq=100000 Hz
10 kHz bandwidth, 10,000 Hz sample rate
CS16 IQ samples
        │
        ▼
ubersdr_loran (Go binary)
  ├── PCM packet decoder (zstd + full/minimal headers)
  ├── Loran-C decoder (decoder.go)
  │     ├── GRI bucket index: bn = floor(fmod(samp-offset, sampPerGRI))
  │     ├── Power: pwr = re² + im²
  │     └── Averaging: CMA / EMA / IIR
  └── HTTP + WebSocket server (server.go)
        │
        ▼
Browser scope UI (static/index.html + loran_c.js)
  ├── Canvas scope: cyan (ch0) / violet (ch1)
  ├── Emission-delay legend bars per station
  └── Controls: GRI, gain, averaging algorithm & parameter
```

The IQ stream is requested at `mode=iq` (10 kHz bandwidth, 10,000 Hz sample rate), which gives `sampPerGRI = 10000 × GRI/100000` buckets per GRI period — e.g. GRI 8000 → 800 buckets. Two independent GRI chains are decoded simultaneously.

---

## Supported Loran-C chains

| GRI  | Chain |
|------|-------|
| 5960 | North Russia (Chayka) |
| 5990 | Caucasus |
| 5991 | USA west coast (eLoran test) |
| 6000 | China BPL Pucheng |
| 6731 | Anthorn UK |
| 6780 | China South Sea |
| 7430 | China North Sea |
| 7950 | Eastern Russia (Chayka) |
| 8000 | Western Russia (Chayka) |
| 8390 | China East Sea |
| 8830 | Saudi Arabia North |
| 8970 | USA east coast (eLoran test) |
| 9930 | Korea |
| 9960 | USA east coast (eLoran test) |

Emission delay data (used to draw station marker bars) courtesy of Markus Vester, DF6NM / [LoranView](http://df6nm.bplaced.net/LoranView/LoranGrabber.htm).

---

## Quick install (Docker)

```bash
curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_loran/main/install.sh | bash
```

This will:
1. Create `~/ubersdr/loran/`
2. Download `docker-compose.yml` and helper scripts
3. Pull the Docker image and start the container

The scope UI will be available at **http://localhost:6088/**

### UberSDR addon proxy configuration

Add the following in the UberSDR Admin → Addon Proxies:

| Field | Value |
|-------|-------|
| Name | `loran` |
| Host | `loran` |
| Port | `6088` |
| Enabled | ✅ |
| Strip prefix | ✅ |
| Rewrite WebSocket origin | ❌ |
| Rate limit | `60` |

The scope will then be accessible at `/addon/loran/` through UberSDR.

---

## Configuration

Edit `~/ubersdr/loran/docker-compose.yml` and set environment variables:

```yaml
environment:
  UBERSDR_URL: "http://ubersdr:8080"   # Required: UberSDR base URL
  # PASS: ""                           # Optional: bypass password
  # WEB_PORT: "8095"                   # Optional: scope UI port (default: 8095)
```

Then restart:
```bash
cd ~/ubersdr/loran && ./restart.sh
```

---

## Management scripts

| Script | Action |
|--------|--------|
| `./start.sh` | Start the container |
| `./stop.sh` | Stop the container |
| `./restart.sh` | Restart the container |
| `./update.sh` | Pull latest image and restart |

---

## Building from source

### Prerequisites

- Go 1.21+

```bash
git clone https://github.com/madpsy/ubersdr_loran.git
cd ubersdr_loran
./build.sh
```

The binary `ubersdr_loran` will be created in the current directory.

### Usage

```
ubersdr_loran [flags]

  -url        string   UberSDR base URL, e.g. http://host:8080  (required)
  -pass       string   Bypass password (optional)
  -web-port   int      Port for the scope web UI (default: 8095)
  -web-static string   Path to static web files (default: ./static)
  -no-reconnect        Disable auto-reconnect on disconnect
```

Example:
```bash
./ubersdr_loran -url http://192.168.1.100:8080 -web-port 8095
```

Then open **http://localhost:8095/** in your browser.

### Building the Docker image

```bash
./docker.sh build        # linux/amd64
./docker.sh arm64        # linux/arm64
./docker.sh push         # multi-arch push to registry
```

---

## Using the scope

### Controls

- **GRI** — enter a GRI value directly (e.g. `8000`) or select a chain from the dropdown
- **Gain** — `0` = auto-scale (default); drag right to apply manual dB attenuation
- **Averaging** — three algorithms:
  - **CMA** (Cumulative Moving Average) — averages over N complete GRI cycles, then resets
  - **EMA** (Exponential Moving Average) — `avg += (pwr - avg) / decay` — smooth, no reset
  - **IIR** — signal-dependent gain: `iir_gain = 1 - exp(-param × pwr/max)` — emphasises strong pulses

### Aligning the master station

Loran-C chains have a **master station** (M) that transmits a 9th pulse. Click anywhere on the scope to shift the display so that position becomes the left edge, aligning the master pulse to the "M" slot in the legend.

### Reading the legend

Coloured bars below each scope trace show the expected positions of each station's pulses based on published emission delays:

- 🔴 Red — Master (M)
- 🟡 Yellow — Secondary W
- 🟢 Green — Secondary X
- 🔵 Blue — Secondary Y
- ⚫ Grey — Secondary Z

---

## Technical notes

### Decoder accuracy

The Go decoder is a line-by-line port of the KiwiSDR C++ extension (`loran_c.cpp`). Key implementation details:

- **Sample counter** uses `uint32` to match the C++ `u4_t` type — wraps at 2³² (~119 hours at 10 kHz), with `fmod` handling the wrap correctly
- **Bucket index**: `bn = floor(fmod(uint32(samp) - uint32(offset), sampPerGRI))` — unsigned subtraction wraps identically to C
- **IIR gain**: `1.0 - exp(-avgParamF × pwr / 32767.0)` where 32767 = max CS16 int16 value (equivalent to `CUTESDR_MAX_VAL` in KiwiSDR)
- **CMA navgs** initialises to `^uint32(0)` (−1 as uint32), incremented to 0 on the first `bn==0`, so the first bucket correctly averages as `pwr / 1`
- **Scope send** fires once per second (`dspSamps > iSrate`) at `bn==0`, before the averaging update for that sample

### Wire protocol

Binary frames sent to the browser:

```
[0:3]  "DAT"     (3 bytes, ASCII)
[3]    cmd       (0 = SCOPE_DATA, 1 = SCOPE_RESET)
[4]    ch        (0 or 1)
[5…]   scope     (nbucket bytes, 0-255 normalised power)
```

Text frames (JSON):

```json
{ "type": "ms_per_bin", "ms_per_bin": 0.1 }
```

Control messages from browser to server (JSON):

```json
{ "type": "set_gri",       "ch": 0, "gri": 8000 }
{ "type": "set_gain",      "ch": 0, "gain": 0 }
{ "type": "set_offset",    "ch": 0, "offset": 42 }
{ "type": "set_avg_algo",  "ch": 0, "algo": 1 }
{ "type": "set_avg_param", "ch": 0, "param": 256.0 }
{ "type": "start" }
```

---

## Credits

- **KiwiSDR Loran-C extension** — John Seamons, ZL4VO/KF6VO — original C++ decoder and JavaScript UI
- **Emission delay data** — Markus Vester, DF6NM — [LoranView](http://df6nm.bplaced.net/LoranView/LoranGrabber.htm)
- **UberSDR** — [madpsy/ka9q_ubersdr](https://github.com/madpsy/ka9q_ubersdr)

---

## Licence

MIT
