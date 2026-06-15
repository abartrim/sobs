package main

import (
	"sort"
	"strings"
)

// inventoryVersionsByPackage mirrors app.py _inventory_versions_by_package: map
// "ecosystem::package" -> set of currently observed versions across the merged library
// inventory. Items missing package/ecosystem/version (after trimming) are skipped.
func (s *server) inventoryVersionsByPackage() map[string]map[string]struct{} {
	out := map[string]map[string]struct{}{}
	for _, it := range s.collectLibraryInventory() {
		pkg := strings.TrimSpace(it.pkg)
		eco := strings.TrimSpace(it.ecosystem)
		ver := strings.TrimSpace(it.version)
		if pkg == "" || eco == "" || ver == "" {
			continue
		}
		key := eco + "::" + pkg
		set, ok := out[key]
		if !ok {
			set = map[string]struct{}{}
			out[key] = set
		}
		set[ver] = struct{}{}
	}
	return out
}

// effectiveCveDisposition mirrors app.py _effective_cve_disposition. A `fixed` disposition
// auto-expires to `open` (returning expired=true) once a different version for the same
// package+ecosystem appears in the merged inventory. All other dispositions pass through.
func effectiveCveDisposition(rawDisposition, pkg, ecosystem, version string,
	versionsByPackage map[string]map[string]struct{}) (string, bool) {
	disposition := rawDisposition
	if disposition == "" {
		disposition = "open"
	}
	if disposition != "fixed" {
		return disposition, false
	}
	current := versionsByPackage[ecosystem+"::"+pkg]
	for v := range current {
		if v != version {
			return "open", true
		}
	}
	return disposition, false
}

// cveDispositionEntry holds a disposition row's resolved disposition + note.
type cveDispositionEntry struct {
	disposition string
	note        string
}

// loadCveDispositions mirrors the dispositions_by_key map built in app.py's CVE read
// endpoints: key "OsvId::Package::Ecosystem::Version" -> {disposition (default open), note}.
func (s *server) loadCveDispositions() map[string]cveDispositionEntry {
	out := map[string]cveDispositionEntry{}
	res, err := s.db.Execute(
		"SELECT OsvId, Package, Ecosystem, Version, Disposition, Note FROM sobs_cve_dispositions FINAL")
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		key := cStr(m, "OsvId") + "::" + cStr(m, "Package") + "::" + cStr(m, "Ecosystem") + "::" + cStr(m, "Version")
		disp := cStr(m, "Disposition")
		if disp == "" {
			disp = "open"
		}
		out[key] = cveDispositionEntry{disposition: disp, note: cStr(m, "Note")}
	}
	return out
}

// cveSplitIds mirrors `[c for c in str(r).split(",") if c]` — split on comma, drop empties.
func cveSplitIds(raw string) []any {
	parts := strings.Split(raw, ",")
	out := []any{}
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// trimmedNonEmpty mirrors `[s.strip() for s in values if s.strip()]` — the getlist filter
// the CVE page applies to repeated severity/ecosystem query args.
func trimmedNonEmpty(values []string) []string {
	out := []string{}
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// toStringSet builds a set from a string slice (mirrors Python set()).
func toStringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

// sortedStringSet mirrors Python sorted({...}) — lexicographic order, returned as []any so
// the template iterates the same sequence the empty-stub []any{} would.
func sortedStringSet(set map[string]struct{}) []any {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// toAnySlice copies a string slice into []any for template rendering.
func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
