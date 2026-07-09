# Embedded GeoIP country dataset (`geoip_country.bin.gz`)

This blob backs `geoip.go`, the stdlib-only Go port of geoip2fast's IP→country
resolution used by `/api/web-traffic/geo` (Python `_geo_lookup_batch`,
`app.py:16194`). It contains **NO third-party Go runtime dependency** — only
`go:embed` + the standard library decode and query it.

## Source

The data is a **byte-exact flattening of geoip2fast's own bundled country
database**, `geoip2fast/geoip2fast.dat.gz` from
<https://github.com/rabuchaim/geoip2fast> (geoip2fast is MIT licensed). That
file's embedded `info` field is `MAXMIND:GeoLite2-Country-IPv4-en-YYYYMMDD`
(GeoLite2 Country, IPv4). It is the exact DB `GeoIP2Fast()` loads by default in
Python, so the Go port reproduces the *same library's* output rather than a
substitute dataset.

geoip2fast's `.dat.gz` is a gzip-compressed pickle:

    [__DAT_VERSION__, source_info, totalNetworks, mainDatabase]
    mainDatabase = [mainIndex, mainListNamesCountry,
                    mainListFirstIP, mainListIDCountryCodes, mainListNetlength]

Each "chunk" is a network: `mainListFirstIP[r][c]` is the first IP (uint32),
`mainListNetlength[r][c]` the prefix length, `mainListIDCountryCodes[r][c]` an
index into `mainListNamesCountry` (`"CC:Country Name"` strings). Indices `< 16`
are reserved/private networks; geoip2fast forces their `country_code` to `--`
and sets `is_private = True`. A query that lands past a chunk's last IP (a gap)
returns `country_code "--"`, `country_name "<not found in database>"`,
`is_private False`.

## Conversion (how this blob was produced)

1. Decode every chunk into `(firstIP, lastIP, countryIdx)` with
   `lastIP = firstIP + 2^(32-prefix) - 1`.
2. Sort by `firstIP`, merge adjacent same-country ranges
   (587,882 → 352,444 ranges).
3. Insert a **gap sentinel** (`idx = 0xFFFF`, "not found") for every hole, so
   the ranges cover the whole 32-bit space and each range's `lastIP` is implicit
   (`= nextFirst - 1`). → 362,973 entries.
4. Pack:

       magic "G2F1"
       uint16 nameCount; repeat{ uint8 ccLen, cc, uint16 nameLen, name }   # "CC","Name"
       uint32 entryCount; repeat{ uvarint(deltaFirst), uint16 idx }

5. gzip (level 9). Result ≈ 525 KB compressed (≈ 1.38 MB raw).

## Verification

The flattened table was checked against
`geoip2fast.GeoIP2Fast().lookup(ip)` over **1.68M sampled IPv4 addresses** —
every range boundary, every gap edge, plus tens of thousands of random IPs —
with **ZERO mismatches** on `(country_code, country_name, is_private)`. The Go
runtime is then exercised by `geoip_test.go` against
`testdata/geo_parity_corpus.json`, a 235-row corpus emitted from the same
library (public, private, reserved, not-found, and IPv6 inputs).

## Updating

Re-download geoip2fast's `geoip2fast.dat.gz`, re-run the decode/flatten/pack
steps above, regenerate `testdata/geo_parity_corpus.json` from the library, and
confirm `go test ./cmd/sobs/` stays green. The on-disk format is versioned by
the `G2F1` magic.
