package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// JS source-map remapping — a faithful port of app.py's _maybe_demangle_js_stack /
// _sourcemap_lookup_for_file / _remap_rum_console_stacks. When SOBS_SOURCE_MAP_ENABLE is set,
// minified stack frames in /v1/errors and RUM events are remapped to original source locations
// using `.map` files under SOBS_SOURCE_MAP_DIR. Python delegates the source-map decode to the
// `sourcemap` PyPI package; this port decodes the Source Map v3 mappings (base64 VLQ) with the Go
// stdlib. Disabled (the default, and the parity corpus) -> a strict identity transform.

// _STACK_FRAME_RE (app.py): prefix, url, :line, :col, suffix.
var stackFrameRe = regexp.MustCompile(
	`^(?P<prefix>.*?)(?P<url>https?://[^\s\)]+|/[^\s\):]+\.js(?:\?[^\s\)]*)?)(?::(?P<line>\d+))(?::(?P<col>\d+))(?P<suffix>.*)$`)

type sourceMapper struct {
	enable bool
	dir    string
	mu     sync.Mutex
	cache  map[string]cachedSourceMap
}

type cachedSourceMap struct {
	mtime int64
	sm    *parsedSourceMap
}

func loadSourceMapper() *sourceMapper {
	return &sourceMapper{
		enable: envFlag("SOBS_SOURCE_MAP_ENABLE", false),
		dir:    strings.TrimSpace(os.Getenv("SOBS_SOURCE_MAP_DIR")),
		cache:  map[string]cachedSourceMap{},
	}
}

// demangleStack mirrors _maybe_demangle_js_stack: per line, match a stack frame and rewrite it to
// "{prefix}[mapped] {target}{suffix}" when a source-map lookup succeeds.
func (sm *sourceMapper) demangleStack(text string) string {
	if text == "" || sm == nil || !sm.enable {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, raw := range lines {
		m := stackFrameRe.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		prefix := m[stackFrameRe.SubexpIndex("prefix")]
		urlStr := m[stackFrameRe.SubexpIndex("url")]
		suffix := m[stackFrameRe.SubexpIndex("suffix")]
		line, err1 := strconv.Atoi(m[stackFrameRe.SubexpIndex("line")])
		col, err2 := strconv.Atoi(m[stackFrameRe.SubexpIndex("col")])
		if err1 != nil || err2 != nil {
			continue
		}
		src, srcLine, srcCol, name, ok := sm.lookupForFile(urlStr, line, col)
		if !ok {
			continue
		}
		target := urlStr + ":" + strconv.Itoa(line) + ":" + strconv.Itoa(col)
		if src != "" {
			target = src + ":" + strconv.Itoa(srcLine) + ":" + strconv.Itoa(srcCol)
		}
		if name != "" {
			target = name + " (" + target + ")"
		}
		lines[i] = prefix + "[mapped] " + target + suffix
	}
	return strings.Join(lines, "\n")
}

// remapRumConsoleStacks mirrors _remap_rum_console_stacks: remap breadcrumbs.console[].stack.
func (sm *sourceMapper) remapRumConsoleStacks(event map[string]any) {
	if sm == nil || !sm.enable {
		return
	}
	bc, ok := event["breadcrumbs"].(map[string]any)
	if !ok {
		return
	}
	entries, ok := bc["console"].([]any)
	if !ok {
		return
	}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if stack := toStr(entry["stack"]); stack != "" {
			entry["stack"] = sm.demangleStack(stack)
		}
	}
}

// lookupForFile mirrors _sourcemap_lookup_for_file: resolve a `.map` for the JS url, parse it
// (mtime-cached), look up (line-1, col-1), and return 1-based original (src, line, col, name).
func (sm *sourceMapper) lookupForFile(jsURL string, line, col int) (string, int, int, string, bool) {
	if !sm.enable || sm.dir == "" {
		return "", 0, 0, "", false
	}
	if info, err := os.Stat(sm.dir); err != nil || !info.IsDir() {
		return "", 0, 0, "", false
	}
	u, _ := url.Parse(strings.TrimSpace(jsURL))
	urlPath := ""
	if u != nil {
		urlPath = u.Path
	}
	relPath := strings.TrimPrefix(urlPath, "/")
	basename := path.Base(urlPath)

	var candidates []string
	if relPath != "" {
		candidates = append(candidates, filepath.Join(sm.dir, relPath+".map"))
	}
	if basename != "" && basename != "." && basename != "/" {
		candidates = append(candidates, filepath.Join(sm.dir, basename+".map"))
		if strings.HasSuffix(basename, ".min.js") {
			candidates = append(candidates, filepath.Join(sm.dir, strings.Replace(basename, ".min.js", ".js.map", 1)))
		}
		if strings.HasSuffix(basename, ".js") {
			candidates = append(candidates, filepath.Join(sm.dir, basename[:len(basename)-3]+".js.map"))
		}
	}

	mapPath := ""
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			mapPath = c
			break
		}
	}
	if mapPath == "" {
		return "", 0, 0, "", false
	}
	parsed := sm.load(mapPath)
	if parsed == nil {
		return "", 0, 0, "", false
	}
	src, origLine, origCol, name, ok := parsed.lookup(maxInt(0, line-1), maxInt(0, col-1))
	if !ok {
		return "", 0, 0, "", false
	}
	return src, origLine + 1, origCol + 1, name, true
}

func (sm *sourceMapper) load(mapPath string) *parsedSourceMap {
	info, err := os.Stat(mapPath)
	if err != nil {
		return nil
	}
	mtime := info.ModTime().UnixNano()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if c, ok := sm.cache[mapPath]; ok && c.mtime == mtime {
		return c.sm
	}
	data, err := os.ReadFile(mapPath)
	if err != nil {
		return nil
	}
	parsed := parseSourceMap(data)
	if parsed == nil {
		return nil
	}
	sm.cache[mapPath] = cachedSourceMap{mtime: mtime, sm: parsed}
	return parsed
}

// ---- Source Map v3 decoding (base64 VLQ) -----------------------------------------------------

type smSegment struct {
	genCol, srcIdx, origLine, origCol, nameIdx int
}

type parsedSourceMap struct {
	sources []string
	names   []string
	lines   [][]smSegment // segments per generated line, ordered by genCol
}

func parseSourceMap(data []byte) *parsedSourceMap {
	var raw struct {
		Sources  []string `json:"sources"`
		Names    []string `json:"names"`
		Mappings string   `json:"mappings"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	return &parsedSourceMap{
		sources: raw.Sources,
		names:   raw.Names,
		lines:   parseMappings(raw.Mappings),
	}
}

func (p *parsedSourceMap) lookup(line, col int) (string, int, int, string, bool) {
	if line < 0 || line >= len(p.lines) {
		return "", 0, 0, "", false
	}
	segs := p.lines[line]
	best := -1
	for i, s := range segs {
		if s.genCol <= col {
			best = i
		} else {
			break
		}
	}
	if best < 0 {
		return "", 0, 0, "", false
	}
	s := segs[best]
	if s.srcIdx < 0 {
		return "", 0, 0, "", false
	}
	src := ""
	if s.srcIdx < len(p.sources) {
		src = p.sources[s.srcIdx]
	}
	name := ""
	if s.nameIdx >= 0 && s.nameIdx < len(p.names) {
		name = p.names[s.nameIdx]
	}
	return src, s.origLine, s.origCol, name, true
}

func parseMappings(mappings string) [][]smSegment {
	var out [][]smSegment
	srcIdx, origLine, origCol, nameIdx := 0, 0, 0, 0 // cumulative across the whole map
	for _, lineStr := range strings.Split(mappings, ";") {
		genCol := 0 // resets each generated line
		var segs []smSegment
		for _, segStr := range strings.Split(lineStr, ",") {
			if segStr == "" {
				continue
			}
			v := decodeVLQSegment(segStr)
			if len(v) == 0 {
				continue
			}
			genCol += v[0]
			seg := smSegment{genCol: genCol, srcIdx: -1, origLine: -1, origCol: -1, nameIdx: -1}
			if len(v) >= 4 {
				srcIdx += v[1]
				origLine += v[2]
				origCol += v[3]
				seg.srcIdx, seg.origLine, seg.origCol = srcIdx, origLine, origCol
			}
			if len(v) >= 5 {
				nameIdx += v[4]
				seg.nameIdx = nameIdx
			}
			segs = append(segs, seg)
		}
		out = append(out, segs)
	}
	return out
}

const vlqB64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var vlqB64Inv = func() [256]int {
	var t [256]int
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(vlqB64); i++ {
		t[vlqB64[i]] = i
	}
	return t
}()

// decodeVLQSegment decodes all base64-VLQ numbers in one mapping segment.
func decodeVLQSegment(seg string) []int {
	var out []int
	pos := 0
	for pos < len(seg) {
		result, shift := 0, 0
		for {
			if pos >= len(seg) {
				return out
			}
			d := vlqB64Inv[seg[pos]]
			pos++
			if d < 0 {
				return out
			}
			result += (d & 0x1f) << shift
			if d&0x20 == 0 {
				break
			}
			shift += 5
		}
		value := result >> 1
		if result&1 != 0 {
			value = -value
		}
		out = append(out, value)
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
