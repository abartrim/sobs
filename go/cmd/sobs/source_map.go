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

	"github.com/sobs/sobs/internal/jsonenc"
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
	// app.py iterates text.splitlines() (splits on \r\n, \r, \n; no trailing empty element) and
	// rejoins with "\n". Match the universal-newline boundaries rather than only "\n".
	lines := rumSplitlines(text)
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

// remapRumConsoleStacksObj is remapRumConsoleStacks for an ordered *jsonenc.Object event (the
// shape ingest_rum parses RUM bodies into). It remaps breadcrumbs.console[].stack in place via
// Set, so the mutation is reflected when the event object is serialized to Body.
func (sm *sourceMapper) remapRumConsoleStacksObj(event *jsonenc.Object) {
	if sm == nil || !sm.enable || event == nil {
		return
	}
	bcv, _ := event.Get("breadcrumbs")
	bc, ok := bcv.(*jsonenc.Object)
	if !ok {
		return
	}
	consoleV, _ := bc.Get("console")
	entries, ok := consoleV.([]any)
	if !ok {
		return
	}
	for _, e := range entries {
		entry, ok := e.(*jsonenc.Object)
		if !ok {
			continue
		}
		sv, _ := entry.Get("stack")
		if stack, ok := sv.(string); ok && stack != "" {
			entry.Set("stack", sm.demangleStack(stack))
		}
	}
}

// safeMapPath joins safeDir (already absolute) with rel and reports whether the resolved
// absolute path is still contained within safeDir, matching the "resolve then verify
// containment" pattern for untrusted path components with multiple segments (as opposed to
// a single-component name, which is instead checked for separators/".." before use).
func safeMapPath(safeDir, rel string) (string, bool) {
	joined := filepath.Join(safeDir, rel)
	absPath, err := filepath.Abs(joined)
	if err != nil {
		return "", false
	}
	if absPath != safeDir && !strings.HasPrefix(absPath, safeDir+string(os.PathSeparator)) {
		return "", false
	}
	return absPath, true
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
	// jsURL comes from a stack-trace frame in externally-submitted RUM/error telemetry, so
	// urlPath is untrusted input. safeDir is validated against sm.dir before building any
	// candidate path from it below.
	safeDir, err := filepath.Abs(sm.dir)
	if err != nil {
		return "", 0, 0, "", false
	}
	relPath := strings.TrimPrefix(filepath.Clean("/"+urlPath), "/")
	basename := path.Base(urlPath)
	if basename != "" {
		basename = strings.TrimPrefix(filepath.Clean("/"+basename), "/")
	}

	var candidates []string
	if relPath != "" {
		if c, ok := safeMapPath(safeDir, relPath+".map"); ok {
			candidates = append(candidates, c)
		}
	}
	// basename must be a single path component (path.Base never returns one containing "/",
	// but a defensive check costs nothing) — reject anything that could still smuggle a
	// directory traversal before it's ever joined onto safeDir.
	if basename != "" && basename != "." && basename != "/" &&
		!strings.Contains(basename, "/") && !strings.Contains(basename, "\\") && !strings.Contains(basename, "..") {
		if c, ok := safeMapPath(safeDir, basename+".map"); ok {
			candidates = append(candidates, c)
		}
		if strings.HasSuffix(basename, ".min.js") {
			if c, ok := safeMapPath(safeDir, strings.Replace(basename, ".min.js", ".js.map", 1)); ok {
				candidates = append(candidates, c)
			}
		}
		if strings.HasSuffix(basename, ".js") {
			if c, ok := safeMapPath(safeDir, basename[:len(basename)-3]+".js.map"); ok {
				candidates = append(candidates, c)
			}
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

// lookup mirrors sourcemap.objects.SourceMapIndex.lookup(line, column) from the `sourcemap` PyPI
// package (mattrobenolt/python-sourcemap, sourcemap/objects.py). The lib's algorithm verbatim:
//
//	try: return self.index[(line, column)]          # exact (line, col) match
//	except KeyError: pass
//	line_index = self.line_index[line]              # IndexError if line out of range
//	i = bisect_right(line_index, column)
//	if not i: raise IndexError                       # column precedes ALL tokens on the line
//	column = line_index[i - 1]                       # nearest token at-or-before column
//	return self.index[(line, column)]
//
// app.py wraps `index.lookup(...)` in `try/except Exception: return None`, so a raised IndexError
// (out-of-range line, OR column before the first token on the line) maps to a failed lookup. There
// is NO cross-line fallback: the search is confined to the requested generated line. When `column`
// is past every token on the line, bisect_right returns len(line_index) and `i-1` selects the LAST
// token on that line (handled by the best-so-far loop below). The decoder (sourcemap/decoder.py)
// builds a Token for EVERY segment, including 1-value segments that carry no source mapping: those
// get Token(src=empty-string default, name=None). app.py computes src = str(getattr(token,"src","") or "")
// -> "" and STILL returns a non-None tuple, i.e. a source-less token is a SUCCESSFUL lookup whose
// empty src makes _maybe_demangle_js_stack fall back to the raw "{url}:{line}:{col}" target. Hence a
// source-less hit returns ok=true with an empty src/name rather than failing.
func (p *parsedSourceMap) lookup(line, col int) (string, int, int, string, bool) {
	if line < 0 || line >= len(p.lines) {
		// self.line_index[line] -> IndexError -> caller's except -> None.
		return "", 0, 0, "", false
	}
	segs := p.lines[line]
	// bisect_right(line_index, col) then take i-1: the last segment with genCol <= col. Segments
	// are appended in decode order, so ties on genCol resolve to the last-stored segment, matching
	// the Python dict `index[(line, col)] = token` (last write wins) combined with bisect.
	best := -1
	for i, s := range segs {
		if s.genCol <= col {
			best = i
		} else {
			break
		}
	}
	if best < 0 {
		// bisect_right == 0 -> `if not i: raise IndexError` -> caller's except -> None.
		return "", 0, 0, "", false
	}
	s := segs[best]
	if s.srcIdx < 0 {
		// 1-value segment: Token(src='' default, name=None). app.py yields src="", name="" and
		// returns a successful (non-None) lookup. The decoder's accumulated src_line/src_col are
		// irrelevant once src is empty (target falls back to "{url}:{line}:{col}"), so return zeros.
		return "", 0, 0, "", true
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
