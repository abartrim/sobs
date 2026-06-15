package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"time"
)

// Settings at-rest encryption — a faithful port of app.py's _encrypt_secret_value /
// _decrypt_secret_value. Sensitive AI settings (the AI API key, GitHub tokens) are encrypted
// before persistence when SOBS_SETTINGS_ENCRYPTION_KEY (or _FILE) is configured. The key is
// sha256(secret) used as a Fernet key (spec-exact, so tokens written by a migrating Python
// deployment decrypt here too). Unset secret -> a strict no-op (plaintext), exactly as in Python,
// so the parity corpus (which never sets the key) stores/reads plaintext unchanged.

const settingsEncPrefix = "enc:v1:"

// readEnvOrFile mirrors app.py _read_env_or_file: the direct env var wins, else the `*_FILE`
// mounted-secret path. (Distinct from readFileOrEnv, which is file-first.)
func readEnvOrFile(envName, fileEnvName string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	if fileEnvName == "" {
		return ""
	}
	fp := strings.TrimSpace(os.Getenv(fileEnvName))
	if fp == "" {
		return ""
	}
	if data, err := os.ReadFile(fp); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

// isSensitiveAISettingKey mirrors app.py _is_sensitive_ai_setting_key.
func isSensitiveAISettingKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	return k == "ai.api_key" || k == "ai.github_token" || strings.HasPrefix(k, "ai.github_token.repo.")
}

// encryptSecretValue mirrors app.py _encrypt_secret_value: no-op when value or secret is empty, or
// when already prefixed; otherwise Fernet-encrypt and prefix. On any failure it returns the value
// unchanged (matching Python's except-path).
func (s *server) encryptSecretValue(value string) string {
	secret := s.cfg.EncryptionSecret
	if value == "" || secret == "" {
		return value
	}
	if strings.HasPrefix(value, settingsEncPrefix) {
		return value
	}
	key := sha256.Sum256([]byte(secret))
	token, err := fernetEncryptWithIV(key[:], value, time.Now().Unix(), nil)
	if err != nil {
		return value
	}
	return settingsEncPrefix + token
}

// decryptSecretValue mirrors app.py _decrypt_secret_value: pass through unprefixed values; an
// encrypted value with no key resolves to "" (and is logged in Python); a bad token -> "".
func (s *server) decryptSecretValue(value string) string {
	if value == "" {
		return value
	}
	if !strings.HasPrefix(value, settingsEncPrefix) {
		return value
	}
	secret := s.cfg.EncryptionSecret
	if secret == "" {
		return ""
	}
	key := sha256.Sum256([]byte(secret))
	pt, err := fernetDecrypt(key[:], value[len(settingsEncPrefix):])
	if err != nil {
		return ""
	}
	return pt
}

// ---- Fernet (https://github.com/fernet/spec), stdlib-only ------------------------------------
//
// Token = base64url( 0x80 || timestamp(8, big-endian) || IV(16) || AES-128-CBC ciphertext ||
// HMAC-SHA256(32) ). The 32-byte key splits into signingKey = key[:16], encKey = key[16:].

func fernetEncryptWithIV(key []byte, plaintext string, ts int64, iv []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("fernet key must be 32 bytes")
	}
	signingKey, encKey := key[:16], key[16:]
	if iv == nil {
		iv = make([]byte, 16)
		if _, err := rand.Read(iv); err != nil {
			return "", err
		}
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad([]byte(plaintext), aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	msg := make([]byte, 0, 1+8+16+len(ct))
	msg = append(msg, 0x80)
	var tsb [8]byte
	binary.BigEndian.PutUint64(tsb[:], uint64(ts))
	msg = append(msg, tsb[:]...)
	msg = append(msg, iv...)
	msg = append(msg, ct...)

	mac := hmac.New(sha256.New, signingKey)
	mac.Write(msg)
	token := append(msg, mac.Sum(nil)...)
	return base64.URLEncoding.EncodeToString(token), nil
}

func fernetDecrypt(key []byte, token string) (string, error) {
	if len(key) != 32 {
		return "", errors.New("fernet key must be 32 bytes")
	}
	data, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}
	if len(data) < 1+8+16+32 || data[0] != 0x80 {
		return "", errors.New("invalid fernet token")
	}
	signingKey, encKey := key[:16], key[16:]
	macStart := len(data) - 32
	msg, mac := data[:macStart], data[macStart:]
	h := hmac.New(sha256.New, signingKey)
	h.Write(msg)
	if !hmac.Equal(h.Sum(nil), mac) {
		return "", errors.New("invalid fernet signature")
	}
	iv := data[9:25]
	ct := data[25:macStart]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", errors.New("invalid fernet ciphertext")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	unpadded, err := pkcs7Unpad(pt, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(unpadded), nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	n := len(data)
	if n == 0 || n%blockSize != 0 {
		return nil, errors.New("invalid pkcs7 length")
	}
	pad := int(data[n-1])
	if pad == 0 || pad > blockSize || pad > n {
		return nil, errors.New("invalid pkcs7 padding")
	}
	for _, b := range data[n-pad:] {
		if int(b) != pad {
			return nil, errors.New("invalid pkcs7 padding")
		}
	}
	return data[:n-pad], nil
}
