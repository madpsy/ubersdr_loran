// chains.go — Loran-C GRI chain database
//
// Single source of truth for all chain metadata including transmitter
// coordinates (WGS-84 decimal degrees). Served to the browser via
// GET /api/chains so the frontend has zero hardcoded domain knowledge.
//
// Coordinates sourced from published ITU/USCG/eLoran data.
// Stations with unknown positions (ex-sites, decommissioned) have Lat=0, Lon=0
// and are skipped by the TDOA/LOP engine.

package main

// Station describes one transmitter within a Loran-C chain.
type Station struct {
	ID      string  `json:"id"`       // "M", "W", "X", "Y", "Z"
	Name    string  `json:"name"`     // human-readable transmitter name
	DelayUS float64 `json:"delay_us"` // emission delay in microseconds after master
	Lat     float64 `json:"lat"`      // WGS-84 decimal degrees (0 = unknown)
	Lon     float64 `json:"lon"`      // WGS-84 decimal degrees (0 = unknown)
}

// Chain describes one Loran-C GRI chain.
type Chain struct {
	GRI      uint32    `json:"gri"`      // Group Repetition Interval (e.g. 8000)
	Name     string    `json:"name"`     // full name
	Short    [2]string `json:"short"`    // two-line abbreviated name for display
	Stations []Station `json:"stations"` // ordered M, W, X, Y, Z
}

// ChainDB is the complete list of known Loran-C / Chayka chains.
// Order matches the channel index assignment in the decoder (ch 0 = ChainDB[0], etc.).
var ChainDB = []Chain{
	{
		GRI:   5960,
		Name:  "North Russia (Chayka)",
		Short: [2]string{"North Russia", "(Chayka)"},
		Stations: []Station{
			{ID: "M", Name: "Inta", DelayUS: 0, Lat: 66.05, Lon: 60.08},
			{ID: "X", Name: "Tumanny Pen", DelayUS: 14670.15, Lat: 69.08, Lon: 35.68},
			{ID: "Z", Name: "Norilsk", DelayUS: 45915.33, Lat: 69.35, Lon: 88.07},
		},
	},
	{
		GRI:   5990,
		Name:  "Caucasus",
		Short: [2]string{"Caucasus", ""},
		Stations: []Station{
			{ID: "M", Name: "Caucasian Center", DelayUS: 0, Lat: 44.15, Lon: 44.02},
			{ID: "X", Name: "Caucasian West", DelayUS: 16587, Lat: 44.97, Lon: 38.87},
			{ID: "Y", Name: "Caucasian East", DelayUS: 31304, Lat: 44.02, Lon: 50.03},
			{ID: "Z", Name: "Caucasian North", DelayUS: 46440, Lat: 46.02, Lon: 44.97},
		},
	},
	{
		GRI:   5991,
		Name:  "USA west coast (eLoran)",
		Short: [2]string{"USA west coast", "(eLoran)"},
		Stations: []Station{
			// Variable secondaries; only master position known
			{ID: "M", Name: "George | Variable: Fallon, Havre", DelayUS: 0, Lat: 0, Lon: 0},
		},
	},
	{
		GRI:   6000,
		Name:  "China BPL Pucheng",
		Short: [2]string{"China BPL", "Pucheng"},
		Stations: []Station{
			{ID: "M", Name: "Pucheng", DelayUS: 0, Lat: 34.57, Lon: 109.53},
		},
	},
	{
		GRI:   6731,
		Name:  "Anthorn UK",
		Short: [2]string{"Anthorn UK", ""},
		Stations: []Station{
			{ID: "M", Name: "Anthorn", DelayUS: 0, Lat: 54.912, Lon: -3.277},
			{ID: "Y", Name: "Anthorn", DelayUS: 27300.00, Lat: 54.912, Lon: -3.277},
		},
	},
	{
		GRI:   6780,
		Name:  "China South Sea",
		Short: [2]string{"China Sea", "South"},
		Stations: []Station{
			{ID: "M", Name: "Hexian", DelayUS: 0, Lat: 24.68, Lon: 111.57},
			{ID: "X", Name: "Raoping", DelayUS: 14464.69, Lat: 23.72, Lon: 116.98},
			{ID: "Y", Name: "Chongzuo", DelayUS: 26925.76, Lat: 22.38, Lon: 107.37},
		},
	},
	{
		GRI:   7430,
		Name:  "China North Sea",
		Short: [2]string{"China Sea", "North"},
		Stations: []Station{
			{ID: "M", Name: "Rongcheng", DelayUS: 0, Lat: 37.17, Lon: 122.42},
			{ID: "X", Name: "Xuancheng", DelayUS: 13459.70, Lat: 30.95, Lon: 118.75},
			{ID: "Y", Name: "Helong", DelayUS: 30852.32, Lat: 42.55, Lon: 129.00},
		},
	},
	{
		GRI:   7950,
		Name:  "Eastern Russia (Chayka)",
		Short: [2]string{"Eastern Russia", "(Chayka)"},
		Stations: []Station{
			{ID: "M", Name: "Aleksandrovsk", DelayUS: 0, Lat: 50.90, Lon: 142.17},
			{ID: "W", Name: "Petropavlovsk", DelayUS: 14506.50, Lat: 53.02, Lon: 158.65},
			{ID: "X", Name: "Ussuriisk", DelayUS: 33678.00, Lat: 43.80, Lon: 132.00},
			{ID: "Y", Name: "(ex-Tokachibuto)", DelayUS: 49104.15, Lat: 0, Lon: 0},
			{ID: "Z", Name: "Okhotsk", DelayUS: 64102.05, Lat: 59.37, Lon: 143.20},
		},
	},
	{
		GRI:   8000,
		Name:  "Western Russia (Chayka)",
		Short: [2]string{"Western Russia", "(Chayka)"},
		Stations: []Station{
			{ID: "M", Name: "Bryansk", DelayUS: 0, Lat: 53.25, Lon: 34.37},
			{ID: "W", Name: "Petrozavodsk", DelayUS: 13217.21, Lat: 61.78, Lon: 34.35},
			{ID: "X", Name: "Slonim", DelayUS: 27125.00, Lat: 53.08, Lon: 25.32},
			{ID: "Y", Name: "Simferopol", DelayUS: 53070.25, Lat: 44.92, Lon: 34.10},
			{ID: "Z", Name: "Syzran", DelayUS: 67941.60, Lat: 53.15, Lon: 48.47},
		},
	},
	{
		GRI:   8390,
		Name:  "China East Sea",
		Short: [2]string{"China Sea", "East"},
		Stations: []Station{
			{ID: "M", Name: "Xuancheng", DelayUS: 0, Lat: 30.95, Lon: 118.75},
			{ID: "X", Name: "Raoping", DelayUS: 13795.52, Lat: 23.72, Lon: 116.98},
			{ID: "Y", Name: "Rongcheng", DelayUS: 31459.70, Lat: 37.17, Lon: 122.42},
		},
	},
	{
		GRI:   8830,
		Name:  "Saudi Arabia North",
		Short: [2]string{"Saudi Arabia", "North"},
		Stations: []Station{
			{ID: "M", Name: "Afif", DelayUS: 0, Lat: 23.92, Lon: 42.93},
			{ID: "W", Name: "Salwa", DelayUS: 13645.00, Lat: 24.75, Lon: 50.82},
			{ID: "X", Name: "(ex-Al Khamasin)", DelayUS: 27265.00, Lat: 0, Lon: 0},
			{ID: "Y", Name: "Ash Shaykh", DelayUS: 42645.00, Lat: 18.28, Lon: 42.57},
			{ID: "Z", Name: "Al Muwassam", DelayUS: 58790.00, Lat: 17.47, Lon: 44.13},
		},
	},
	{
		GRI:   8970,
		Name:  "USA east coast (eLoran)",
		Short: [2]string{"USA east coast", "(eLoran)"},
		Stations: []Station{
			{ID: "M", Name: "Wildwood", DelayUS: 0, Lat: 39.00, Lon: -74.87},
		},
	},
	{
		GRI:   9930,
		Name:  "Korea",
		Short: [2]string{"Korea", ""},
		Stations: []Station{
			{ID: "M", Name: "Pohang", DelayUS: 0, Lat: 36.02, Lon: 129.37},
			{ID: "W", Name: "Kwang Ju", DelayUS: 11946.97, Lat: 35.12, Lon: 126.92},
			{ID: "X", Name: "(ex-Gesashi)", DelayUS: 25565.52, Lat: 0, Lon: 0},
			{ID: "Y", Name: "(ex-Niijima)", DelayUS: 40085.64, Lat: 0, Lon: 0},
			{ID: "Z", Name: "Ussuriisk", DelayUS: 54162.44, Lat: 43.80, Lon: 132.00},
		},
	},
	{
		GRI:   9960,
		Name:  "USA east coast (eLoran)",
		Short: [2]string{"USA east coast", "(eLoran)"},
		Stations: []Station{
			{ID: "M", Name: "Wildwood", DelayUS: 0, Lat: 39.00, Lon: -74.87},
		},
	},
}
