package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// RUM asset upload — a faithful port of app.py's ingest_rum_asset (POST /v1/rum/assets) and its
// config (RUM_ASSET_SIGNING_KEY / _SIGN_WINDOW_SEC / _MAX_BYTES). The real upload path activates
// only when SOBS_RUM_ASSET_SIGNING_KEY is configured; when unset (the parity corpus, where the
// key is unconfigured) verifyRumAssetSignature short-circuits to the same
// 503 "Asset upload signing key is not configured" Python emits — so the captured
// post__v1_rum_assets case is byte-for-byte unchanged. The matching download route
// (GET /v1/rum/assets/<id>) lives in handlers_get2.go (handleV1RumAssetByID), unaffected here.

type rumAssetConfig struct {
	signingKey    string
	signWindowSec int
	maxBytes      int
}

func loadRumAssetConfig() rumAssetConfig {
	return rumAssetConfig{
		signingKey:    os.Getenv("SOBS_RUM_ASSET_SIGNING_KEY"),
		signWindowSec: envInt("SOBS_RUM_ASSET_SIGN_WINDOW_SEC", 300),
		maxBytes:      envInt("SOBS_RUM_ASSET_MAX_BYTES", 8*1024*1024),
	}
}

var (
	rumAssetNameStripRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
	rumAssetTypeStripRe = regexp.MustCompile(`[^a-z0-9._-]+`)
	rumAssetExtRe       = regexp.MustCompile(`^\.[a-zA-Z0-9]{1,8}$`)
)

// posixBasename mirrors os.path.basename on POSIX: the substring after the last '/'.
// (Unlike filepath.Base it returns "" for "", and does not treat '\\' as a separator.)
func posixBasename(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// sanitizeRumAssetName mirrors app.py _sanitize_rum_asset_name.
func sanitizeRumAssetName(value string) string {
	raw := posixBasename(strings.TrimSpace(value))
	if raw == "" {
		return "asset"
	}
	cleaned := strings.Trim(rumAssetNameStripRe.ReplaceAllString(raw, "-"), "-._")
	if cleaned == "" {
		return "asset"
	}
	return cleaned
}

// sanitizeRumAssetType mirrors app.py _sanitize_rum_asset_type.
func sanitizeRumAssetType(value string) string {
	raw := strings.ToLower(strings.TrimSpace(value))
	if raw == "" {
		return "asset"
	}
	cleaned := strings.Trim(rumAssetTypeStripRe.ReplaceAllString(raw, "-"), "-._")
	if cleaned == "" {
		return "asset"
	}
	return cleaned
}

// assetExtension mirrors app.py _asset_extension.
func assetExtension(assetName, contentType string) string {
	ext := filepath.Ext(assetName)
	if ext != "" && rumAssetExtRe.MatchString(ext) {
		return strings.ToLower(strings.TrimPrefix(ext, "."))
	}
	mapping := map[string]string{
		"application/json":         "json",
		"application/octet-stream": "bin",
		"text/plain":               "txt",
		"image/png":                "png",
		"image/jpeg":               "jpg",
		"image/webp":               "webp",
		"video/webm":               "webm",
	}
	key := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if v, ok := mapping[key]; ok {
		return v
	}
	return "bin"
}

// rumAssetSignature mirrors app.py _rum_asset_signature (hex HMAC-SHA256).
func rumAssetSignature(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// rumAssetSignaturePayload mirrors app.py _rum_asset_signature_payload (newline-joined fields).
func rumAssetSignaturePayload(method, path, timestamp, bodySHA256, contentType, assetType, assetName string) string {
	return strings.Join([]string{
		strings.ToUpper(method),
		path,
		timestamp,
		bodySHA256,
		strings.ToLower(strings.TrimSpace(contentType)),
		strings.ToLower(strings.TrimSpace(assetType)),
		assetName,
	}, "\n")
}

func intAbs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// verifyRumAssetSignature mirrors app.py _verify_rum_asset_signature. Returns (ok, errMsg).
func (s *server) verifyRumAssetSignature(r *http.Request, body []byte, method, path, contentType, assetType, assetName string) (bool, string) {
	if s.rumAsset.signingKey == "" {
		return false, "Asset upload signing key is not configured"
	}

	timestamp := strings.TrimSpace(r.Header.Get("X-SOBS-Asset-Timestamp"))
	signature := strings.ToLower(strings.TrimSpace(r.Header.Get("X-SOBS-Asset-Signature")))
	if timestamp == "" || signature == "" {
		return false, "Missing asset signature headers"
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false, "Invalid asset signature timestamp"
	}

	now := nowUTC().Unix()
	window := s.rumAsset.signWindowSec
	if window < 1 {
		window = 1
	}
	if intAbs64(now-ts) > int64(window) {
		return false, "Asset signature timestamp outside allowed window"
	}

	bodySum := sha256.Sum256(body)
	payload := rumAssetSignaturePayload(method, path, timestamp, hex.EncodeToString(bodySum[:]), contentType, assetType, assetName)
	expected := rumAssetSignature(s.rumAsset.signingKey, payload)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return false, "Invalid asset signature"
	}
	return true, ""
}

// handleRumAssetUpload mirrors app.py ingest_rum_asset (POST /v1/rum/assets). It is invoked from
// handleV1IngestGet after require_api_key has already passed. Parity-safe: with the signing key
// unset (the corpus), verifyRumAssetSignature returns the same 503 the prior stub emitted, before
// any storage or randomness is touched.
func (s *server) handleRumAssetUpload(w http.ResponseWriter, r *http.Request) {
	assetType := sanitizeRumAssetType(rumAssetQueryDefault(r, "type", "asset"))
	assetName := sanitizeRumAssetName(rumAssetQueryDefault(r, "name", "asset"))

	contentType := strings.TrimSpace(strings.SplitN(headerOr(r, "Content-Type", "application/octet-stream"), ";", 2)[0])
	body, _ := io.ReadAll(r.Body)

	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "asset body is required"))
		return
	}
	maxBytes := s.rumAsset.maxBytes
	if maxBytes < 1024 {
		maxBytes = 1024
	}
	if len(body) > maxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, jsonenc.NewObject().Set("error", "asset exceeds max allowed size"))
		return
	}

	ok, errMsg := s.verifyRumAssetSignature(r, body, r.Method, r.URL.Path, contentType, assetType, assetName)
	if !ok {
		if strings.Contains(errMsg, "not configured") {
			writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("error", errMsg))
			return
		}
		writeJSON(w, http.StatusUnauthorized, jsonenc.NewObject().Set("error", errMsg))
		return
	}

	assetID := newUUIDHex()
	ext := assetExtension(assetName, contentType)
	storageName := assetID + "." + ext
	dir := filepath.Join(s.cfg.DataDir, "rum_assets")
	_ = os.MkdirAll(dir, 0o755)
	assetPath := filepath.Join(dir, storageName)
	metaPath := filepath.Join(dir, assetID+".meta.json")

	if err := os.WriteFile(assetPath, body, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("error", "asset write failed"))
		return
	}

	// metadata mirrors Python json.dump(ensure_ascii=False): a compact, insertion-ordered object
	// (jsonenc.Compact = SortKeys:false, EnsureASCII:false, "," / ":") that the download route re-reads.
	meta := jsonenc.NewObject().
		Set("id", assetID).
		Set("type", assetType).
		Set("original_name", assetName).
		Set("storage_name", storageName).
		Set("content_type", contentType).
		Set("size", len(body)).
		Set("uploaded_at", nowISO())
	if err := os.WriteFile(metaPath, jsonenc.Encode(meta, jsonenc.Compact), 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("error", "asset metadata write failed"))
		return
	}

	assetURL, _ := s.urlFor([]any{"rum_asset_download"}, map[string]any{"asset_id": assetID}, []string{"asset_id"})
	writeJSON(w, http.StatusCreated, jsonenc.NewObject().
		Set("id", assetID).
		Set("type", assetType).
		Set("name", assetName).
		Set("contentType", contentType).
		Set("size", len(body)).
		Set("url", assetURL))
}

// rumAssetQueryDefault mirrors request.args.get(key, default): the default applies only when the
// param is absent; a present-but-empty param yields "".
func rumAssetQueryDefault(r *http.Request, key, def string) string {
	if r.URL.Query().Has(key) {
		return r.URL.Query().Get(key)
	}
	return def
}

// headerOr mirrors (request.headers.get(name) or default): default for an absent OR empty header.
func headerOr(r *http.Request, name, def string) string {
	if v := r.Header.Get(name); v != "" {
		return v
	}
	return def
}
