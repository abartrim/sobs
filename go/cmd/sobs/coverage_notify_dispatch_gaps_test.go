package main

// coverage_notify_dispatch_gaps_test.go — fault-injection tests for notify_dispatch.go's
// external-I/O error branches. notify_dispatch_test.go already covers the crypto primitives
// (HKDF, VAPID JWT, push-payload round trip) and the config-validation short-circuits; the
// golden corpus only ever exercises the webhook channel's happy path, so the SMTP dial/
// STARTTLS/AUTH failure branches, the VAPID-key-loading parse-failure branches, and the
// upstream-HTTP error/non-2xx branches were never reached. sendSMTPMessage dials a real
// net.Conn (not swappable via a package var), so these use a minimal in-process fake SMTP
// server over a loopback listener — no real network, no mocking framework, just stdlib
// net/net/textproto — plus the existing SOBS_UPSTREAM_FIXTURES seam the corpus harness itself
// uses for the HTTP-based channels.

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// fakeSMTPServer starts a minimal SMTP server on loopback. cmdOverride maps a command verb
// (EHLO, STARTTLS, AUTH, MAIL, RCPT, DATA) to a canned response line instead of the default
// success reply; DATA's override, if set, is sent in place of "354 go ahead" (so the message
// body is never entered). received captures the full DATA body of the first accepted message.
func fakeSMTPServer(t *testing.T, cmdOverride map[string]string) (addr string, received *string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var body string
	received = &body
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				tp := textproto.NewConn(conn)
				_ = tp.PrintfLine("220 fake smtp ready")
				for {
					line, err := tp.ReadLine()
					if err != nil {
						return
					}
					upper := strings.ToUpper(line)
					switch {
					case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
						_ = tp.PrintfLine("250-fake.smtp greets you")
						_ = tp.PrintfLine("250-STARTTLS")
						_ = tp.PrintfLine("250 AUTH PLAIN")
					case strings.HasPrefix(upper, "STARTTLS"):
						if resp, ok := cmdOverride["STARTTLS"]; ok {
							_ = tp.PrintfLine(resp)
							continue
						}
						_ = tp.PrintfLine("220 go ahead")
						return // real TLS handshake unsupported by this fake; tests using it only assert the error path
					case strings.HasPrefix(upper, "AUTH"):
						if resp, ok := cmdOverride["AUTH"]; ok {
							_ = tp.PrintfLine(resp)
							continue
						}
						_ = tp.PrintfLine("235 authenticated")
					case strings.HasPrefix(upper, "MAIL"):
						if resp, ok := cmdOverride["MAIL"]; ok {
							_ = tp.PrintfLine(resp)
							continue
						}
						_ = tp.PrintfLine("250 ok")
					case strings.HasPrefix(upper, "RCPT"):
						if resp, ok := cmdOverride["RCPT"]; ok {
							_ = tp.PrintfLine(resp)
							continue
						}
						_ = tp.PrintfLine("250 ok")
					case strings.HasPrefix(upper, "DATA"):
						if resp, ok := cmdOverride["DATA"]; ok {
							_ = tp.PrintfLine(resp)
							continue
						}
						_ = tp.PrintfLine("354 go ahead")
						var lines []string
						for {
							bline, err := tp.ReadLine()
							if err != nil || bline == "." {
								break
							}
							lines = append(lines, bline)
						}
						body = strings.Join(lines, "\n")
						_ = tp.PrintfLine("250 ok")
					case strings.HasPrefix(upper, "QUIT"):
						_ = tp.PrintfLine("221 bye")
						return
					default:
						_ = tp.PrintfLine("250 ok")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), received
}

func TestSendSMTPMessage_DialError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now -> connection refused
	got := sendSMTPMessage(addr, "localhost", false, "", "", "from@x", "to@y", []byte("msg"))
	if got == "ok" {
		t.Fatal("want a dial error, got ok")
	}
}

func TestSendSMTPMessage_FullSuccess(t *testing.T) {
	addr, received := fakeSMTPServer(t, nil)
	got := sendSMTPMessage(addr, "localhost", false, "", "", "from@x.test", "to@y.test", []byte("Subject: hi\r\n\r\nbody text"))
	if got != "ok" {
		t.Fatalf("got %q, want ok", got)
	}
	if !strings.Contains(*received, "body text") {
		t.Errorf("server did not receive the message body: %q", *received)
	}
}

func TestSendSMTPMessage_AuthError(t *testing.T) {
	addr, _ := fakeSMTPServer(t, map[string]string{"AUTH": "535 authentication failed"})
	got := sendSMTPMessage(addr, "localhost", false, "user", "pass", "from@x", "to@y", []byte("msg"))
	if got == "ok" {
		t.Fatal("want an auth error, got ok")
	}
}

func TestSendSMTPMessage_StartTLSError(t *testing.T) {
	addr, _ := fakeSMTPServer(t, map[string]string{"STARTTLS": "502 not supported"})
	got := sendSMTPMessage(addr, "localhost", true, "", "", "from@x", "to@y", []byte("msg"))
	if got == "ok" {
		t.Fatal("want a STARTTLS error, got ok")
	}
}

func TestSendSMTPMessage_MailRcptDataErrors(t *testing.T) {
	cases := []struct {
		name     string
		override map[string]string
	}{
		{"MAIL rejected", map[string]string{"MAIL": "550 sender rejected"}},
		{"RCPT rejected", map[string]string{"RCPT": "550 recipient rejected"}},
		{"DATA rejected", map[string]string{"DATA": "550 no data allowed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, _ := fakeSMTPServer(t, c.override)
			got := sendSMTPMessage(addr, "localhost", false, "", "", "from@x", "to@y", []byte("msg"))
			if got == "ok" {
				t.Fatalf("%s: want an error, got ok", c.name)
			}
		})
	}
}

// TestDispatchEmailChannel_SubjectTruncationAndSuccess drives dispatchEmailChannel end to end
// (config -> SMTP send) to cover its subject-truncation branch (notify_dispatch.go:95-97) and
// the wrapper's message-construction glue, which sendSMTPMessage's direct tests above don't
// reach on their own.
func TestDispatchEmailChannel_SubjectTruncationAndSuccess(t *testing.T) {
	host, port, found := strings.Cut(mustFakeSMTPHostPort(t), ":")
	_ = found
	s := &server{}
	longSummary := strings.Repeat("x", 250)
	config := jsonenc.NewObject().
		Set("smtp_host", host).Set("smtp_port", port).
		Set("to_addr", "to@example.com").Set("from_addr", "from@example.com").
		Set("use_tls", "0")
	payload := jsonenc.NewObject().Set("summary", longSummary)
	got := s.dispatchEmailChannel(config, payload)
	if got != "ok" {
		t.Fatalf("dispatchEmailChannel = %q, want ok", got)
	}
}

// mustFakeSMTPHostPort starts a fake SMTP server (default success script) and returns its
// "host:port" address, for tests that go through dispatchEmailChannel's host/port config split.
func mustFakeSMTPHostPort(t *testing.T) string {
	t.Helper()
	addr, _ := fakeSMTPServer(t, nil)
	return addr
}

// ---- loadVapidPrivateKey ----------------------------------------------------------------

func TestLoadVapidPrivateKey_Unconfigured(t *testing.T) {
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", "")
	s := &server{db: &storetest.FakeDB{}}
	priv, source, err := s.loadVapidPrivateKey()
	if err != nil || priv != nil || source != "" {
		t.Errorf("unconfigured: priv=%v source=%q err=%v, want nil/\"\"/nil", priv, source, err)
	}
}

func TestLoadVapidPrivateKey_InvalidBase64(t *testing.T) {
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", "not-valid-base64!!!")
	s := &server{db: &storetest.FakeDB{}}
	_, _, err := s.loadVapidPrivateKey()
	if err == nil {
		t.Fatal("want a base64 decode error, got nil")
	}
}

func TestLoadVapidPrivateKey_UnparseableShortDER(t *testing.T) {
	// Fewer than 32 bytes and not valid PKCS8/SEC1 DER -> falls through every branch to the
	// final "could not parse" error (notify_dispatch.go:243).
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", base64.RawURLEncoding.EncodeToString([]byte("short")))
	s := &server{db: &storetest.FakeDB{}}
	_, _, err := s.loadVapidPrivateKey()
	if err == nil || !strings.Contains(err.Error(), "could not parse VAPID private key") {
		t.Fatalf("err = %v, want the could-not-parse message", err)
	}
}

func TestLoadVapidPrivateKey_RawScalarFallback(t *testing.T) {
	// >= 32 bytes that aren't valid DER at all -> raw-scalar fallback path (notify_dispatch.go:238-241).
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", base64.RawURLEncoding.EncodeToString(raw))
	s := &server{db: &storetest.FakeDB{}}
	priv, source, err := s.loadVapidPrivateKey()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if priv == nil || source != "env" {
		t.Fatalf("priv=%v source=%q, want a key from env", priv, source)
	}
}

func TestLoadVapidPrivateKey_PKCS8(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", base64.RawURLEncoding.EncodeToString(der))
	s := &server{db: &storetest.FakeDB{}}
	priv, source, err := s.loadVapidPrivateKey()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if priv == nil || !priv.Equal(key) || source != "env" {
		t.Fatalf("priv=%v source=%q, want the original PKCS8 key", priv, source)
	}
}

func TestLoadVapidPrivateKey_SEC1(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", base64.RawURLEncoding.EncodeToString(der))
	s := &server{db: &storetest.FakeDB{}}
	priv, source, err := s.loadVapidPrivateKey()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if priv == nil || !priv.Equal(key) || source != "env" {
		t.Fatalf("priv=%v source=%q, want the original SEC1 key", priv, source)
	}
}

func TestLoadVapidPrivateKey_DBFallback(t *testing.T) {
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", "")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString(der)
	s := &server{db: storetest.SettingsDB(map[string]string{"vapid_private_key": b64})}
	priv, source, err := s.loadVapidPrivateKey()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if priv == nil || source != "db" {
		t.Fatalf("priv=%v source=%q, want a key with source=db", priv, source)
	}
}

// ---- dispatchSlackChannel / dispatchBrowserPushChannel upstream branches ------------------

func TestDispatchSlackChannel_NonOKStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir) // no fixture file written -> upstreamFixture returns 404
	s := &server{}
	config := jsonenc.NewObject().Set("webhook_url", "http://sobs-slack.mock/hooks/x")
	got := s.dispatchSlackChannel(config, jsonenc.NewObject().Set("summary", "hi"))
	if !strings.Contains(got, "HTTP 404") {
		t.Errorf("got %q, want it to mention HTTP 404", got)
	}
}

func TestDispatchSlackChannel_UpstreamError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	url := "http://sobs-slack.mock/hooks/bad-json"
	stem := upstreamFixtureKey("POST", url)
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte("not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	config := jsonenc.NewObject().Set("webhook_url", url)
	got := s.dispatchSlackChannel(config, jsonenc.NewObject().Set("summary", "hi"))
	if got == "ok" {
		t.Fatal("want an error from the malformed fixture, got ok")
	}
}

func TestDispatchBrowserPushChannel_NonOKStatus(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", base64.RawURLEncoding.EncodeToString(der))
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir) // no fixture -> 404, hits the "default" non-2xx branch
	s := &server{db: &storetest.FakeDB{}}
	subPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := jsonenc.NewObject().
		Set("endpoint", "http://sobs-push.mock/endpoint/abc").
		Set("p256dh", base64.RawURLEncoding.EncodeToString(subPriv.PublicKey().Bytes())).
		Set("auth", base64.RawURLEncoding.EncodeToString(make([]byte, 16)))
	got := s.dispatchBrowserPushChannel(config, jsonenc.NewObject().Set("summary", "hi"))
	if !strings.Contains(got, "HTTP 404") {
		t.Errorf("got %q, want it to mention HTTP 404", got)
	}
}

func TestDispatchBrowserPushChannel_InvalidP256dhAndAuth(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", base64.RawURLEncoding.EncodeToString(der))
	s := &server{db: &storetest.FakeDB{}}

	badP256dh := jsonenc.NewObject().
		Set("endpoint", "http://x").Set("p256dh", "not base64!!").Set("auth", base64.RawURLEncoding.EncodeToString(make([]byte, 16)))
	if got := s.dispatchBrowserPushChannel(badP256dh, jsonenc.NewObject()); got != "invalid p256dh" {
		t.Errorf("bad p256dh: got %q, want %q", got, "invalid p256dh")
	}

	badAuth := jsonenc.NewObject().
		Set("endpoint", "http://x").Set("p256dh", base64.RawURLEncoding.EncodeToString(make([]byte, 65))).Set("auth", "not base64!!")
	if got := s.dispatchBrowserPushChannel(badAuth, jsonenc.NewObject()); got != "invalid auth" {
		t.Errorf("bad auth: got %q, want %q", got, "invalid auth")
	}
}

func TestDispatchBrowserPushChannel_NoVapidKeyConfigured(t *testing.T) {
	t.Setenv("SOBS_VAPID_PRIVATE_KEY", "")
	s := &server{db: &storetest.FakeDB{}}
	config := jsonenc.NewObject().
		Set("endpoint", "http://x").
		Set("p256dh", base64.RawURLEncoding.EncodeToString(make([]byte, 65))).
		Set("auth", base64.RawURLEncoding.EncodeToString(make([]byte, 16)))
	got := s.dispatchBrowserPushChannel(config, jsonenc.NewObject())
	if !strings.Contains(got, "VAPID private key is not configured") {
		t.Errorf("got %q, want the not-configured message", got)
	}
}
