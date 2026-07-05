package goldenreplay

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
)

// route mirrors the trimmed per-route entry in testdata/manifest.json (itself derived
// once from the frozen migration/manifest/routes.yaml — see MEMORY / plan notes on the
// SOBS Go cutover for provenance).
type route struct {
	ID      string   `json:"id"`
	Path    string   `json:"path"`
	Methods []string `json:"methods"`
	Profile string   `json:"profile"`
	Request request  `json:"request"`
	Stream  bool     `json:"stream"`
	Mask    []string `json:"mask"`
}

type request struct {
	Method  string            `json:"method"`
	Query   map[string]any    `json:"query"`
	JSON    json.RawMessage   `json:"json"`
	Form    map[string]any    `json:"form"`
	BodyB64 string            `json:"body_b64"`
	Headers map[string]string `json:"headers"`
}

func (r request) hasJSON() bool { return len(r.JSON) > 0 && string(r.JSON) != "null" }

type manifest struct {
	Routes []route `json:"routes"`
}

type exclusionEntry struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type exclusionsFile struct {
	Exclusions []exclusionEntry `json:"exclusions"`
}

func loadManifest(dir string) ([]route, map[string]bool, map[string]map[string]string, error) {
	m := manifest{}
	if err := loadJSON(filepath.Join(dir, "manifest.json"), &m); err != nil {
		return nil, nil, nil, err
	}
	excl := exclusionsFile{}
	if err := loadJSON(filepath.Join(dir, "exclusions.json"), &excl); err != nil {
		return nil, nil, nil, err
	}
	excluded := make(map[string]bool, len(excl.Exclusions))
	for _, e := range excl.Exclusions {
		excluded[e.ID] = true
	}
	profileEnv := map[string]map[string]string{}
	if err := loadJSON(filepath.Join(dir, "fixtures", "profile_env.json"), &profileEnv); err != nil {
		return nil, nil, nil, err
	}
	return m.Routes, excluded, profileEnv, nil
}

// applyExtraFiles writes a profile's non-chdb fixture files (e.g. seed_rumasset's on-disk
// rum_assets/ blobs — see dump_profile_seeds.py's dump_extra_files) into dataDir, base64-decoded,
// at the same relative paths they were captured from.
func applyExtraFiles(filesPath, dataDir string) error {
	var files map[string]string
	if err := loadJSON(filesPath, &files); err != nil {
		return err
	}
	for rel, b64 := range files {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return err
		}
		target := filepath.Join(dataDir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func readGolden(dir, routeID string) (response, bool) {
	base := filepath.Join(dir, "golden", routeID)
	body, err := os.ReadFile(filepath.Join(base, "body.bin"))
	if err != nil {
		return response{}, false
	}
	statusRaw, err := os.ReadFile(filepath.Join(base, "status"))
	if err != nil {
		return response{}, false
	}
	status := 0
	for _, c := range statusRaw {
		if c < '0' || c > '9' {
			break
		}
		status = status*10 + int(c-'0')
	}
	headersRaw, err := os.ReadFile(filepath.Join(base, "headers.txt"))
	if err != nil {
		return response{}, false
	}
	var headers [][2]string
	for _, line := range splitLines(string(headersRaw)) {
		if idx := indexSep(line); idx >= 0 {
			headers = append(headers, [2]string{line[:idx], line[idx+2:]})
		}
	}
	return response{Status: status, Headers: headers, Body: body}, true
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			out = append(out, line)
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func indexSep(s string) int {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == ':' && s[i+1] == ' ' {
			return i
		}
	}
	return -1
}
