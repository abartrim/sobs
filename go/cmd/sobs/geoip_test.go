package main

import (
	"encoding/json"
	"os"
	"testing"
)

// geoParityRow is one entry of testdata/geo_parity_corpus.json — the EXPECTED
// (country, country_code) that the SOBS Python path (_geo_lookup_batch +
// api_web_traffic_geo's "Unknown" default) produces for an IP. The corpus was
// generated against the real geoip2fast library (the same bundled DB embedded
// here); see assets/GEOIP_DATA.md.
type geoParityRow struct {
	IP          string `json:"ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
}

// TestGeoLookupBatchParity asserts the embedded-DB lookup reproduces geoip2fast
// for a representative corpus of public/private/reserved/IPv6 inputs, applying
// the same "" → "Unknown" handler default Python uses.
func TestGeoLookupBatchParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/geo_parity_corpus.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var rows []geoParityRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(rows) < 50 {
		t.Fatalf("corpus too small: %d rows", len(rows))
	}

	ips := make([]string, len(rows))
	for i, r := range rows {
		ips[i] = r.IP
	}
	got := geoLookupBatch(ips, true)

	for _, r := range rows {
		g := got[r.IP]
		country := g.country
		if country == "" {
			country = "Unknown" // api_web_traffic_geo's default (app.py:17737)
		}
		if country != r.Country || g.countryCode != r.CountryCode {
			t.Errorf("ip %q: got (%q,%q) want (%q,%q)",
				r.IP, country, g.countryCode, r.Country, r.CountryCode)
		}
	}
}

// TestGeoLookupBatchEmpty pins the empty-input behavior that the parity corpus
// depends on: no IPs and disabled-geo both yield an empty result map, so the
// handler emits empty country_counts/ip_details exactly as Python.
func TestGeoLookupBatchEmpty(t *testing.T) {
	if got := geoLookupBatch(nil, true); len(got) != 0 {
		t.Errorf("nil ips: want empty, got %v", got)
	}
	if got := geoLookupBatch([]string{"8.8.8.8"}, false); len(got) != 0 {
		t.Errorf("geo disabled: want empty, got %v", got)
	}
}

// TestGeoDBLoads ensures the embedded blob decodes and carries a country table.
func TestGeoDBLoads(t *testing.T) {
	db := geoLookupDB()
	if db == nil {
		t.Fatal("embedded geoip blob failed to decode")
	}
	if len(db.firsts) == 0 || len(db.firsts) != len(db.idx) {
		t.Fatalf("range arrays bad: firsts=%d idx=%d", len(db.firsts), len(db.idx))
	}
	if len(db.countries) < 200 {
		t.Fatalf("country table too small: %d", len(db.countries))
	}
	// 8.8.8.8 is a stable sentinel for "US" (a public, non-private match).
	code, name, priv := db.lookup4(0x08080808)
	if priv || code != "US" || name != "United States" {
		t.Fatalf("8.8.8.8 lookup: got (%q,%q,priv=%v)", code, name, priv)
	}
	// A gap address returns geoip2fast's not-found result.
	gcode, gname, gpriv := db.lookup4(0xCB007105) // 203.0.113.5 (TEST-NET-3 gap in DB)
	if gpriv || gcode != geoNotFoundCode || gname != geoNotFoundName {
		t.Fatalf("gap lookup: got (%q,%q,priv=%v)", gcode, gname, gpriv)
	}
}

// TestIsPrivateIP pins the private/public partition ported from _is_private_ip.
func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"10.0.0.1": true, "172.16.5.4": true, "192.168.1.1": true,
		"127.0.0.1": true, "169.254.0.5": true, "0.0.0.0": true,
		"::1": true, "fe80::1": true, "fc00::1": true, "not-an-ip": true,
		"8.8.8.8": false, "1.1.1.1": false, "2001:4860:4860::8888": false,
	}
	for ip, want := range cases {
		if got := isPrivateIP(ip); got != want {
			t.Errorf("isPrivateIP(%q)=%v want %v", ip, got, want)
		}
	}
}
