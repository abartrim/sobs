package main

import (
	"sort"
	"strings"
	"sync"
)

// ingest_attr_keys.go ports app.py's _remember_attr_keys / _remember_log_attr_keys side-effect:
// every ingest inserter records the distinct attribute keys it discovered into sobs_log_attr_keys,
// keyed by record_type ("log"/"span"/"resource"/"scope"). The discovered keys feed the
// /api/logs/field-hints reader (SELECT DISTINCT AttrKey ... WHERE RecordType='log') and the
// tag-rule attribute-key suggestion endpoint.
//
// Faithful 1:1 with app.py:2198 _remember_attr_keys, including the in-memory cache + lock that
// (a) primes from the DB once, (b) skips keys already known so the same key is not re-inserted,
// and (c) caps the total tracked set at LOG_ATTR_KEYS_MAX. The cache mirrors Python's module-level
// _log_attr_keys_by_record_type so repeated ingests in one process write the same rows Python would.

const logAttrKeysMaxDefault = 20000

var attrKeyRecordTypes = []string{"log", "span", "resource", "scope"}

type attrKeyCache struct {
	mu      sync.Mutex
	loaded  bool
	byType  map[string]map[string]struct{}
	maxKeys int
}

var logAttrKeyCache = &attrKeyCache{
	byType:  map[string]map[string]struct{}{},
	maxKeys: envInt("SOBS_LOG_ATTR_KEYS_MAX", logAttrKeysMaxDefault),
}

// prime mirrors _prime_log_attr_key_cache: load DISTINCT AttrKey per record_type from the DB once.
func (c *attrKeyCache) prime(s *server) {
	if c.loaded {
		return
	}
	for _, rt := range attrKeyRecordTypes {
		set := map[string]struct{}{}
		res, err := s.db.Execute(
			"SELECT DISTINCT AttrKey FROM sobs_log_attr_keys FINAL WHERE RecordType=? AND IsDeleted=0 ORDER BY AttrKey",
			rt)
		if err == nil {
			for _, m := range rowMaps(res) {
				k := strings.TrimSpace(cStr(m, "AttrKey"))
				if k != "" {
					set[k] = struct{}{}
				}
			}
		}
		c.byType[rt] = set
	}
	c.loaded = true
}

// rememberAttrKeys mirrors app.py _remember_attr_keys: discover the new keys across the given attr
// maps for record_type, persist them to sobs_log_attr_keys, and update the in-memory cache. Called
// from inside the ingest write op (the insert is via insertRowsNormalized, like Python).
func (s *server) rememberAttrKeys(attrsMaps []map[string]any, recordType string) {
	if len(attrsMaps) == 0 {
		return
	}
	c := logAttrKeyCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prime(s)

	existing := c.byType[recordType]
	if existing == nil {
		existing = map[string]struct{}{}
		c.byType[recordType] = existing
	}
	if len(existing) >= c.maxKeys {
		return
	}

	candidates := map[string]struct{}{}
	for _, attrs := range attrsMaps {
		for rawKey := range attrs {
			key := strings.TrimSpace(rawKey)
			if key == "" {
				continue
			}
			if _, ok := existing[key]; ok {
				continue
			}
			if _, ok := candidates[key]; ok {
				continue
			}
			if len(existing)+len(candidates) >= c.maxKeys {
				break
			}
			candidates[key] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return
	}

	sortedKeys := make([]string, 0, len(candidates))
	for k := range candidates {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	version := fixedVersionMillis()
	rows := make([]map[string]any, 0, len(sortedKeys))
	for idx, key := range sortedKeys {
		rows = append(rows, map[string]any{
			"RecordType": recordType,
			"AttrKey":    key,
			"IsDeleted":  0,
			"Version":    version + int64(idx),
		})
	}
	if _, err := s.insertRowsNormalized("sobs_log_attr_keys", rows); err != nil {
		return
	}
	for k := range candidates {
		existing[k] = struct{}{}
	}
}

// rememberLogAttrKeys mirrors _remember_log_attr_keys (record_type defaults to "log").
func (s *server) rememberLogAttrKeys(attrsMaps []map[string]any) {
	s.rememberAttrKeys(attrsMaps, "log")
}

// extractAttrMaps mirrors _extract_attr_maps: pull the named attribute field (a string map) from
// each inserted row. Rows carry their attr maps as map[string]any (the OTel Map columns).
func extractAttrMaps(rows []map[string]any, field string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if m, ok := row[field].(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// extractLogAttrMaps mirrors _extract_log_attr_maps (field "LogAttributes").
func extractLogAttrMaps(rows []map[string]any) []map[string]any {
	return extractAttrMaps(rows, "LogAttributes")
}
