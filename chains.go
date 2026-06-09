// chains.go — Loran-C GRI chain database
//
// This is the single source of truth for all chain metadata.
// It is served to the browser via GET /api/chains so the frontend
// has zero hardcoded domain knowledge.

package main

// Station describes one transmitter within a Loran-C chain.
type Station struct {
	ID      string  `json:"id"`       // "M", "W", "X", "Y", "Z"
	Name    string  `json:"name"`     // human-readable transmitter name
	DelayUS float64 `json:"delay_us"` // emission delay in microseconds after master
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
			{ID: "M", Name: "Inta", DelayUS: 0},
			{ID: "X", Name: "Tumanny Pen", DelayUS: 14670.15},
			{ID: "Z", Name: "Norilsk", DelayUS: 45915.33},
		},
	},
	{
		GRI:   5990,
		Name:  "Caucasus",
		Short: [2]string{"Caucasus", ""},
		Stations: []Station{
			{ID: "M", Name: "Caucasian Center", DelayUS: 0},
			{ID: "X", Name: "Caucasian West", DelayUS: 16587},
			{ID: "Y", Name: "Caucasian East", DelayUS: 31304},
			{ID: "Z", Name: "Caucasian North", DelayUS: 46440},
		},
	},
	{
		GRI:   5991,
		Name:  "USA west coast (eLoran)",
		Short: [2]string{"USA west coast", "(eLoran)"},
		Stations: []Station{
			{ID: "M", Name: "George | Variable: Fallon, Havre", DelayUS: 0},
		},
	},
	{
		GRI:   6000,
		Name:  "China BPL Pucheng",
		Short: [2]string{"China BPL", "Pucheng"},
		Stations: []Station{
			{ID: "M", Name: "Pucheng", DelayUS: 0},
		},
	},
	{
		GRI:   6731,
		Name:  "Anthorn UK",
		Short: [2]string{"Anthorn UK", ""},
		Stations: []Station{
			{ID: "M", Name: "Anthorn", DelayUS: 0},
			{ID: "Y", Name: "Anthorn", DelayUS: 27300.00},
		},
	},
	{
		GRI:   6780,
		Name:  "China South Sea",
		Short: [2]string{"China Sea", "South"},
		Stations: []Station{
			{ID: "M", Name: "Hexian", DelayUS: 0},
			{ID: "X", Name: "Raoping", DelayUS: 14464.69},
			{ID: "Y", Name: "Chongzuo", DelayUS: 26925.76},
		},
	},
	{
		GRI:   7430,
		Name:  "China North Sea",
		Short: [2]string{"China Sea", "North"},
		Stations: []Station{
			{ID: "M", Name: "Rongcheng", DelayUS: 0},
			{ID: "X", Name: "Xuancheng", DelayUS: 13459.70},
			{ID: "Y", Name: "Helong", DelayUS: 30852.32},
		},
	},
	{
		GRI:   7950,
		Name:  "Eastern Russia (Chayka)",
		Short: [2]string{"Eastern Russia", "(Chayka)"},
		Stations: []Station{
			{ID: "M", Name: "Aleksandrovsk", DelayUS: 0},
			{ID: "W", Name: "Petropavlovsk", DelayUS: 14506.50},
			{ID: "X", Name: "Ussuriisk", DelayUS: 33678.00},
			{ID: "Y", Name: "(ex-Tokachibuto)", DelayUS: 49104.15},
			{ID: "Z", Name: "Okhotsk", DelayUS: 64102.05},
		},
	},
	{
		GRI:   8000,
		Name:  "Western Russia (Chayka)",
		Short: [2]string{"Western Russia", "(Chayka)"},
		Stations: []Station{
			{ID: "M", Name: "Bryansk", DelayUS: 0},
			{ID: "W", Name: "Petrozavodsk", DelayUS: 13217.21},
			{ID: "X", Name: "Slonim", DelayUS: 27125.00},
			{ID: "Y", Name: "Simferopol", DelayUS: 53070.25},
			{ID: "Z", Name: "Syzran", DelayUS: 67941.60},
		},
	},
	{
		GRI:   8390,
		Name:  "China East Sea",
		Short: [2]string{"China Sea", "East"},
		Stations: []Station{
			{ID: "M", Name: "Xuancheng", DelayUS: 0},
			{ID: "X", Name: "Raoping", DelayUS: 13795.52},
			{ID: "Y", Name: "Rongcheng", DelayUS: 31459.70},
		},
	},
	{
		GRI:   8830,
		Name:  "Saudi Arabia North",
		Short: [2]string{"Saudi Arabia", "North"},
		Stations: []Station{
			{ID: "M", Name: "Afif", DelayUS: 0},
			{ID: "W", Name: "Salwa", DelayUS: 13645.00},
			{ID: "X", Name: "(ex-Al Khamasin)", DelayUS: 27265.00},
			{ID: "Y", Name: "Ash Shaykh", DelayUS: 42645.00},
			{ID: "Z", Name: "Al Muwassam", DelayUS: 58790.00},
		},
	},
	{
		GRI:   8970,
		Name:  "USA east coast (eLoran)",
		Short: [2]string{"USA east coast", "(eLoran)"},
		Stations: []Station{
			{ID: "M", Name: "Wildwood", DelayUS: 0},
		},
	},
	{
		GRI:   9930,
		Name:  "Korea",
		Short: [2]string{"Korea", ""},
		Stations: []Station{
			{ID: "M", Name: "Pohang", DelayUS: 0},
			{ID: "W", Name: "Kwang Ju", DelayUS: 11946.97},
			{ID: "X", Name: "(ex-Gesashi)", DelayUS: 25565.52},
			{ID: "Y", Name: "(ex-Niijima)", DelayUS: 40085.64},
			{ID: "Z", Name: "Ussuriisk", DelayUS: 54162.44},
		},
	},
	{
		GRI:   9960,
		Name:  "USA east coast (eLoran)",
		Short: [2]string{"USA east coast", "(eLoran)"},
		Stations: []Station{
			{ID: "M", Name: "Wildwood", DelayUS: 0},
		},
	},
}
