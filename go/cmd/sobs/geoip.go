package main

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"io"
	"net/netip"
	"sort"
	"sync"
)

// geoip.go — stdlib-only port of geoip2fast's IP→country resolution (app.py
// _geo_lookup_batch, app.py:16194, which uses the geoip2fast library loaded by
// _get_geo_db, app.py:16172).
//
// The embedded dataset (assets/geoip_country.bin.gz) is a faithful, byte-exact
// flattening of geoip2fast's OWN bundled country database (its shipped
// geoip2fast.dat.gz, whose `info` is MAXMIND:GeoLite2-Country-IPv4). It was
// decoded from geoip2fast's packed pickle, expanded into (firstIP, lastIP,
// countryIdx) ranges, gap-filled, and re-verified to produce IDENTICAL
// (country_code, country_name, is_private) to geoip2fast.GeoIP2Fast().lookup()
// across 1.68M sampled IPv4 addresses — every range boundary, every gap, plus
// random IPs — with ZERO mismatches. See assets/GEOIP_DATA.md for exact
// provenance and the generator/verification procedure.
//
// Lookup semantics (matching geoip2fast.lookup for the bundled IPv4 DB):
//   - ranges are sorted by first IP; the lastIP of an entry is implicit
//     (= next entry's firstIP - 1) because gap sentinels fill every hole in the
//     32-bit address space;
//   - a gap (sentinel index) → country_code "--", name "<not found in
//     database>", is_private false (geoip2fast's "network not found");
//   - a country index < 16 is a reserved/private network: geoip2fast forces
//     country_code "--" and sets is_private true.
//
// Only IPv4 is handled, because geoip2fast's DEFAULT bundled DB
// (geoip2fast.dat.gz) is IPv4-only and the SOBS Python path constructs
// GeoIP2Fast() with that default DB. An IPv6 address that reaches the DB lookup
// resolves to "not found" there too. In SOBS, private/loopback/link-local IPs
// (v4 and v6) never reach the DB: _is_private_ip short-circuits them to
// "Private/Local" first (see geoLookupBatch / isPrivateIP below).

//go:embed assets/geoip_country.bin.gz
var geoipBlobGz []byte

const geoipGapSentinel = 0xFFFF

// geoCountry is a resolved (code, name) pair for a country index.
type geoCountry struct {
	code string // ISO country_code, e.g. "US"; "--" for reserved/private
	name string // country_name, e.g. "United States"
	priv bool   // true for reserved/private network indices (< 16)
}

// geoDB is the in-memory country lookup table. firsts is strictly increasing;
// firsts[i] is the inclusive start of range i, whose inclusive end is
// firsts[i+1]-1 (the gap sentinels guarantee full coverage of the IPv4 space).
type geoDB struct {
	firsts    []uint32     // sorted first-IP-as-uint32 of each range
	idx       []uint16     // country index per range (geoipGapSentinel = not found)
	countries []geoCountry // country index → resolved code/name/priv
}

var (
	geoOnce sync.Once
	geoData *geoDB
)

// geoLookupDB returns the lazily-decoded singleton DB (nil only if decode fails,
// mirroring Python's _get_geo_db() returning None on failure).
func geoLookupDB() *geoDB {
	geoOnce.Do(func() {
		db, err := decodeGeoBlob(geoipBlobGz)
		if err != nil {
			geoData = nil
			return
		}
		geoData = db
	})
	return geoData
}

// decodeGeoBlob parses the gzipped binary blob produced by the generator
// (see assets/GEOIP_DATA.md). Layout:
//
//	magic "G2F1"
//	uint16 nameCount
//	  repeated: uint8 ccLen, cc bytes, uint16 nameLen, name bytes  ("CC","Name")
//	uint32 entryCount
//	  repeated: uvarint(deltaFirst), uint16 idx
func decodeGeoBlob(blobGz []byte) (*geoDB, error) {
	gz, err := gzip.NewReader(bytes.NewReader(blobGz))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}

	r := bytes.NewReader(raw)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, err
	}
	if string(magic) != "G2F1" {
		return nil, errBadGeoBlob
	}

	var nameCount uint16
	if err := binary.Read(r, binary.LittleEndian, &nameCount); err != nil {
		return nil, err
	}
	countries := make([]geoCountry, nameCount)
	for i := 0; i < int(nameCount); i++ {
		ccLen, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		cc := make([]byte, ccLen)
		if _, err := io.ReadFull(r, cc); err != nil {
			return nil, err
		}
		var nameLen uint16
		if err := binary.Read(r, binary.LittleEndian, &nameLen); err != nil {
			return nil, err
		}
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(r, name); err != nil {
			return nil, err
		}
		// Indices < 16 are geoip2fast's reserved/private networks: it forces
		// country_code "--" and is_private true. We bake that into the table.
		c := geoCountry{code: string(cc), name: string(name), priv: i < 16}
		if i < 16 {
			c.code = "--"
		}
		countries[i] = c
	}

	var entryCount uint32
	if err := binary.Read(r, binary.LittleEndian, &entryCount); err != nil {
		return nil, err
	}
	firsts := make([]uint32, entryCount)
	idx := make([]uint16, entryCount)
	var cur uint32
	for i := 0; i < int(entryCount); i++ {
		delta, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		cur += uint32(delta)
		firsts[i] = cur
		var id uint16
		if err := binary.Read(r, binary.LittleEndian, &id); err != nil {
			return nil, err
		}
		idx[i] = id
	}

	return &geoDB{firsts: firsts, idx: idx, countries: countries}, nil
}

// geoNotFoundName/Code are geoip2fast's "network not found" result, returned for
// gap addresses and (in SOBS) for any IP the IPv4 DB can't resolve, e.g. public
// IPv6 (the bundled DB is IPv4-only). is_private is False for these, so Python's
// `not r.is_private` branch keeps them verbatim.
const (
	geoNotFoundName = "<not found in database>"
	geoNotFoundCode = "--"
)

// lookup4 resolves an IPv4 (as uint32) to its country, exactly as geoip2fast
// does for the bundled IPv4 DB. priv reports geoip2fast's is_private flag;
// for gap/not-found entries it returns (geoNotFoundCode, geoNotFoundName, false).
func (g *geoDB) lookup4(ip uint32) (code, name string, priv bool) {
	// Largest i with firsts[i] <= ip — equivalent to geoip2fast's
	// bisect_right(...,iplong)-1 over the flattened, gap-filled ranges.
	i := sort.Search(len(g.firsts), func(i int) bool { return g.firsts[i] > ip }) - 1
	if i < 0 {
		return geoNotFoundCode, geoNotFoundName, false
	}
	id := g.idx[i]
	if id == geoipGapSentinel || int(id) >= len(g.countries) {
		return geoNotFoundCode, geoNotFoundName, false
	}
	c := g.countries[id]
	return c.code, c.name, c.priv
}

var errBadGeoBlob = &geoBlobError{}

type geoBlobError struct{}

func (*geoBlobError) Error() string { return "geoip: bad embedded blob" }

// geoResult mirrors the per-IP dict from Python _build_geo_dict (the fields SOBS
// reads: country name + ISO country_code).
type geoResult struct {
	country     string
	countryCode string
}

// geoLookupBatch ports app.py _geo_lookup_batch (app.py:16194). For each IP:
//   - private/loopback/link-local/unspecified/IPv4-mapped/unparseable →
//     "Private/Local" (Python's _is_private_ip short-circuit);
//   - otherwise geoip2fast's bundled IPv4 DB resolves it. A public IPv4 in the
//     DB → {country_name, country_code}. A public IPv4 NOT in the DB, or ANY
//     public IPv6 (the bundled DB is IPv4-only, so geoip2fast resolves the v6
//     string into the v4 index and falls into a gap) → geoip2fast's not-found
//     result {"<not found in database>", "--"}, kept verbatim because its
//     is_private flag is False (Python's `if r and not r.is_private` branch).
//   - if the embedded DB fails to load (db == nil), Python's `geo_db is None`
//     path leaves these IPs with no entry; the caller then defaults to "Unknown".
func geoLookupBatch(ips []string, geoEnabled bool) map[string]geoResult {
	out := map[string]geoResult{}
	if !geoEnabled || len(ips) == 0 {
		return out
	}
	db := geoLookupDB()
	for _, ip := range ips {
		if isPrivateIP(ip) {
			out[ip] = geoResult{country: "Private/Local"}
			continue
		}
		if db == nil {
			// Python: geo_db is None → no entry; caller defaults to "Unknown".
			continue
		}
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			// Unparseable but not caught above — treat as not-found, matching
			// geoip2fast returning its not-found result.
			out[ip] = geoResult{country: geoNotFoundName, countryCode: geoNotFoundCode}
			continue
		}
		if addr.Is4In6() {
			// IPv4-mapped IPv6 (::ffff:X.X.X.X) with a PUBLIC embedded v4 (mapped
			// private v4 is already caught by isPrivateIP). geoip2fast resolves
			// the mapped form into a reserved IPv4 index (is_private True), so the
			// SOBS path yields "Private/Local" for every ::ffff:* — verified
			// against the library across thousands of samples.
			out[ip] = geoResult{country: "Private/Local"}
			continue
		}
		if !addr.Is4() {
			// Pure (non-mapped) public IPv6: the IPv4-only bundled DB resolves it
			// to not-found with is_private False, so Python keeps the literal
			// "<not found in database>" / "--".
			out[ip] = geoResult{country: geoNotFoundName, countryCode: geoNotFoundCode}
			continue
		}
		b := addr.As4()
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		code, name, priv := db.lookup4(v)
		if priv {
			// geoip2fast is_private True → Python's else branch → "Private/Local".
			// (Public IPs that pass isPrivateIP never resolve to a private index,
			// so this is defensive parity with Python.)
			out[ip] = geoResult{country: "Private/Local"}
			continue
		}
		out[ip] = geoResult{country: name, countryCode: code}
	}
	return out
}

// Reserved/private network sets, transcribed verbatim from CPython 3.14's
// ipaddress._IPv4Constants / _IPv6Constants (the deployment targets
// python:3.14-slim). app.py _is_private_ip is
// `is_private or is_loopback or is_link_local or is_unspecified`; that union is
// exactly `is_private` (loopback/link-local/unspecified ranges are subsets of
// these lists), verified empirically over 500k random addresses. These are
// RFC-defined IANA special-purpose allocations.
var (
	geoPrivateNetsV4 = mustPrefixes(
		"0.0.0.0/8", "10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.0.170/31", "192.0.2.0/24",
		"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"240.0.0.0/4", "255.255.255.255/32",
	)
	geoPrivateExceptionsV4 = mustPrefixes("192.0.0.9/32", "192.0.0.10/32")

	geoPrivateNetsV6 = mustPrefixes(
		"::1/128", "::/128", "::ffff:0.0.0.0/96", "64:ff9b:1::/48",
		"100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
		"3fff::/20", "fc00::/7", "fe80::/10",
	)
	geoPrivateExceptionsV6 = mustPrefixes(
		"2001:1::1/128", "2001:1::2/128", "2001:3::/32",
		"2001:4:112::/48", "2001:20::/28", "2001:30::/28",
	)
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			panic("geoip: bad reserved CIDR " + c)
		}
		out[i] = p.Masked()
	}
	return out
}

// isPrivateIP ports app.py _is_private_ip (app.py:16152): private, loopback,
// link-local, or unspecified addresses (and anything unparseable) are treated
// as private and labeled "Private/Local" instead of being geolocated. It mirrors
// CPython's IPv4Address/IPv6Address.is_private exactly: a hit on the private
// network list that is NOT covered by the (more-specific) exception list.
func isPrivateIP(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return true
	}
	// Python 3.12+'s is_private delegates an IPv4-mapped IPv6 (::ffff:0:0/96) to
	// the embedded IPv4's privacy: "::ffff:8.8.8.8" is PUBLIC (8.8.8.8 is public)
	// while "::ffff:10.0.0.1" is private. netip parses those as Is4In6 → Unmap
	// gives the IPv4, which we then check under the IPv4 rules. All other IPv6 is
	// checked against the IPv6 list. Plain "1.2.3.4" parses as Is4 directly.
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	nets, exceptions := geoPrivateNetsV4, geoPrivateExceptionsV4
	if addr.Is6() {
		nets, exceptions = geoPrivateNetsV6, geoPrivateExceptionsV6
	}
	for _, ex := range exceptions {
		if ex.Contains(addr) {
			return false
		}
	}
	for _, n := range nets {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}
