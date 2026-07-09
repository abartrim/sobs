// Package goldenreplay is the Go-native successor to migration/tools/parity_check.py: it
// replays the frozen golden corpus (go/testdata/) against the compiled sobs binary, with no
// Python or app.py oracle involved. normalize.go is a faithful port of
// migration/tools/normalize.py's comparison rules — see that file's docstring for the
// philosophy (bodies are NEVER normalized; only transport/wall-clock header artifacts are).
package goldenreplay

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

type response struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

var (
	dropHeaders           = map[string]bool{"date": true, "server": true, "connection": true, "transfer-encoding": true, "keep-alive": true}
	alwaysDropCacheHeader = map[string]bool{"last-modified": true, "expires": true}
	fsEtagRE              = regexp.MustCompile(`^"?\d+\.\d+-\d+-\d+"?$`)
	sessionCookieRE       = regexp.MustCompile(`(?i)^(sobs_session=)([^;]*)(.*)$`)
)

func isBodyless(status int) bool {
	return status == 204 || status == 304 || (status >= 100 && status < 200)
}

func normalize(r response) response {
	bodyless := isBodyless(r.Status)
	out := response{Status: r.Status, Body: r.Body}
	for _, kv := range r.Headers {
		name, value := kv[0], kv[1]
		lname := strings.ToLower(name)
		if dropHeaders[lname] {
			continue
		}
		if lname == "content-length" && bodyless {
			continue
		}
		if alwaysDropCacheHeader[lname] {
			continue
		}
		if lname == "etag" && fsEtagRE.MatchString(strings.TrimSpace(value)) {
			continue
		}
		if lname == "set-cookie" && strings.Contains(value, "sobs_session=") {
			out.Headers = append(out.Headers, [2]string{name, normalizeSessionCookie(value)})
			continue
		}
		if lname == "allow" {
			parts := strings.Split(value, ",")
			methods := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					methods = append(methods, p)
				}
			}
			sort.Strings(methods)
			out.Headers = append(out.Headers, [2]string{name, strings.Join(methods, ", ")})
			continue
		}
		out.Headers = append(out.Headers, [2]string{name, value})
	}
	return out
}

func normalizeSessionCookie(value string) string {
	m := sessionCookieRE.FindStringSubmatch(value)
	if m == nil {
		return value
	}
	cookieVal, rest := m[2], m[3]
	segments := strings.Split(cookieVal, ".")
	var payload string
	if len(segments) >= 3 {
		payload = strings.Join(segments[:len(segments)-2], ".")
	} else {
		payload = segments[0]
	}
	canonical, ok := decodeSessionPayload(payload)
	if !ok {
		return "sobs_session=" + payload + ".<sig>" + rest
	}
	return "sobs_session=" + canonical + ".<sig>" + rest
}

func decodeSessionPayload(payload string) (string, bool) {
	compressed := strings.HasPrefix(payload, ".")
	b64 := payload
	if compressed {
		b64 = payload[1:]
	}
	if m := len(b64) % 4; m != 0 {
		b64 += strings.Repeat("=", 4-m)
	}
	data, err := base64.URLEncoding.DecodeString(b64)
	if err != nil {
		return "", false
	}
	if compressed {
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return "", false
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(zr); err != nil {
			return "", false
		}
		data = buf.Bytes()
	}
	var session any
	if err := json.Unmarshal(data, &session); err != nil {
		return "", false
	}
	canon, err := canonicalJSON(session)
	if err != nil {
		return "", false
	}
	return canon, true
}

// canonicalJSON mirrors Python's json.dumps(sort_keys=True, separators=(",", ":")).
func canonicalJSON(v any) (string, error) {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			vs, err := canonicalJSON(val[k])
			if err != nil {
				return "", err
			}
			b.WriteString(vs)
		}
		b.WriteByte('}')
		return b.String(), nil
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, e := range val {
			if i > 0 {
				b.WriteByte(',')
			}
			vs, err := canonicalJSON(e)
			if err != nil {
				return "", err
			}
			b.WriteString(vs)
		}
		b.WriteByte(']')
		return b.String(), nil
	default:
		b, err := json.Marshal(val)
		return string(b), err
	}
}

func headerMultiset(headers [][2]string) []string {
	out := make([]string, len(headers))
	for i, kv := range headers {
		out[i] = strings.ToLower(kv[0]) + "\x00" + kv[1]
	}
	sort.Strings(out)
	return out
}

func equalMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalResponses is the Go equivalent of normalize.py's equal(): status + order-independent
// header multiset + raw byte-exact body.
func equalResponses(a, b response) bool {
	na, nb := normalize(a), normalize(b)
	if na.Status != nb.Status {
		return false
	}
	if !equalMultiset(headerMultiset(na.Headers), headerMultiset(nb.Headers)) {
		return false
	}
	return bytes.Equal(na.Body, nb.Body)
}

// applyMasks replaces masked byte runs on both sides before comparison — the Go port of
// _apply_masks in parity_check.py. Content-Length is recomputed to the masked body length.
func applyMasks(r response, masks []string) response {
	if len(masks) == 0 {
		return r
	}
	body := r.Body
	compiled := make([]*regexp.Regexp, len(masks))
	for i, m := range masks {
		compiled[i] = regexp.MustCompile(m)
	}
	for _, re := range compiled {
		body = re.ReplaceAll(body, []byte("<MASKED>"))
	}
	headers := make([][2]string, len(r.Headers))
	for i, kv := range r.Headers {
		name, value := kv[0], kv[1]
		if strings.ToLower(name) == "content-length" {
			headers[i] = [2]string{name, jsonInt(len(body))}
			continue
		}
		for _, re := range compiled {
			value = re.ReplaceAllString(value, "<MASKED>")
		}
		headers[i] = [2]string{name, value}
	}
	return response{Status: r.Status, Headers: headers, Body: body}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
