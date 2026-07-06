package integration

import (
	"fmt"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// initThemeScript is injected on every new document to pin dark theme and
// suppress the first-run tour (mirrors the add_init_script calls).
const initThemeScript = `
try {
    localStorage.setItem('sobs-theme', 'dark');
    localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
    localStorage.setItem('sobs.firstRunTourShown.v1', '1');
} catch (_) {}
`

// dismissTourScript force-hides the first-run tour modal if present.
const dismissTourScript = `
() => {
    try {
        localStorage.setItem('sobs-theme', 'dark');
        localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
        localStorage.setItem('sobs.firstRunTourShown.v1', '1');
    } catch (_) {}

    document.documentElement.setAttribute('data-bs-theme', 'dark');

    const doneBtn = document.getElementById('firstRunTourDoneBtn');
    if (doneBtn && doneBtn.offsetParent !== null) {
        doneBtn.click();
    }

    const modalEl = document.getElementById('firstRunTourModal');
    if (modalEl) {
        modalEl.classList.remove('show');
        modalEl.setAttribute('aria-hidden', 'true');
        modalEl.style.display = 'none';
    }

    document.body.classList.remove('modal-open');
    document.body.style.removeProperty('padding-right');
    const backdrop = document.querySelector('.modal-backdrop');
    if (backdrop) backdrop.remove();
}
`

func (p *page) dismissTourModal() { p.MustEval(dismissTourScript) }

func (p *page) screenshot(filename, url string) {
	p.MustEvalOnNewDocument(initThemeScript)
	p.setViewport(1440, 900)
	p.gotoIdle(url)
	p.dismissTourModal()
	p.MustScreenshot(filepath.Join(screenshotsDir, filename))
}

func (p *page) screenshotAtViewport(filename, url string, w, h int) {
	p.MustEvalOnNewDocument(initThemeScript)
	p.setViewport(w, h)
	p.gotoIdle(url)
	p.dismissTourModal()
	p.MustScreenshot(filepath.Join(screenshotsDir, filename))
}

func (p *page) expectVisibleText(substr string) {
	if !p.evalBool(`(s) => document.body.innerText.includes(s)`, substr) {
		p.t.Errorf("expected visible text %q", substr)
	}
}

// Match a trace drilldown link with a non-empty trace_id anywhere in the query
// string. The Go server's url_for emits query params alphabetically (Flask
// preserves kwarg order), so trace_id is not necessarily the first param.
var traceHrefRe = regexp.MustCompile(`href="(/traces\?[^"]*\btrace_id=[a-f0-9]+[^"]*)"`)

func firstTraceDetailURL(t *testing.T) string {
	t.Helper()
	status, text := getText(t, "/traces?limit=200")
	mustStatus(t, status, 200)
	m := traceHrefRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no trace detail href found on /traces")
	}
	rel := replaceAll(m[1], "&amp;", "&")
	return baseURL + rel
}

var dashIDRe = regexp.MustCompile(`/dashboards/([^/?#]+)`)

func createDocsDashboard(t *testing.T) string {
	t.Helper()
	resp := postForm(t, "/dashboards", urlValues(
		"name", "Docs Screenshot Dashboard",
		"description", "Auto-generated dashboard for docs screenshots",
	))
	defer resp.Body.Close()
	if resp.StatusCode != 302 && resp.StatusCode != 303 {
		t.Fatalf("create dashboard status = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	m := dashIDRe.FindStringSubmatch(loc)
	if m == nil {
		t.Fatalf("no dashboard id in Location %q", loc)
	}
	dashboardID := m[1]

	chartSpec := map[string]any{
		"template_id": "custom_echarts",
		"sql": map[string]any{
			"mode": "raw",
			"override_sql": "SELECT toStartOfMinute(TimestampTime) AS time, count() AS value " +
				"FROM otel_logs GROUP BY time ORDER BY time LIMIT 120",
		},
		"visual": map[string]any{
			"custom_mapping_json": jsonString(map[string]any{
				"points": map[string]any{"from": "rows"},
			}),
			"custom_option_json": jsonString(map[string]any{
				"tooltip": map[string]any{"trigger": "axis"},
				"xAxis":   map[string]any{"type": "time"},
				"yAxis":   map[string]any{"type": "value"},
				"series": []any{map[string]any{
					"name":       "Logs/min",
					"type":       "line",
					"data":       "{{points}}",
					"showSymbol": false,
					"smooth":     true,
				}},
			}),
		},
	}

	addResp := postForm(t, "/dashboards/"+dashboardID+"/charts", urlValues(
		"title", "Log Volume by Minute",
		"chart_spec_json", jsonString(chartSpec),
	))
	defer addResp.Body.Close()
	if addResp.StatusCode != 302 && addResp.StatusCode != 303 {
		t.Fatalf("add chart status = %d", addResp.StatusCode)
	}
	return baseURL + "/dashboards/" + dashboardID
}

// maskingSeed holds the sensitive markers seeded for masking assertions.
type maskingSeed struct {
	service, markerToken, replayID, artifactID, email, apiKey, auth, password string
}

func seedMaskingRumError(t *testing.T, marker string) maskingSeed {
	s := maskingSeed{
		email:       fmt.Sprintf("owner+%s@example.com", marker),
		apiKey:      fmt.Sprintf("sk_live_%s_secret", marker),
		auth:        fmt.Sprintf("Authorization: Bearer token-%s", marker),
		password:    fmt.Sprintf("secret-%s", marker),
		service:     fmt.Sprintf("masking-playwright-%s", marker),
		markerToken: fmt.Sprintf("mask-marker-%s", marker),
		replayID:    fmt.Sprintf("replay-%s", marker),
		artifactID:  fmt.Sprintf("shot-%s", marker),
	}
	payload := []any{map[string]any{
		"type":        "error",
		"service":     s.service,
		"timestamp":   "2026-04-10T00:00:00Z",
		"sessionId":   fmt.Sprintf("sess-%s", marker),
		"traceId":     fmt.Sprintf("trace-%s", marker),
		"spanId":      "0123456789abcdef",
		"url":         fmt.Sprintf("https://example.com/app?email=%s", s.email),
		"message":     fmt.Sprintf("Mask check %s service=%s %s api_key=%s password=%s", s.markerToken, s.service, s.auth, s.apiKey, s.password),
		"errorType":   "TypeError",
		"errorSource": "window.onerror",
		"stack":       fmt.Sprintf("TypeError: Mask check for %s", s.email),
		"artifact": map[string]any{
			"type": "screenshot",
			"id":   s.artifactID,
			"url":  fmt.Sprintf("https://example.com/artifacts/shot-%s.png?owner=%s&api_key=%s", marker, s.email, s.apiKey),
		},
		"replay": map[string]any{
			"id":  s.replayID,
			"url": fmt.Sprintf("https://example.com/replays/replay-%s.json?authorization=%s&email=%s", marker, s.auth, s.email),
		},
	}}
	status, _ := postJSON(t, "/v1/rum", payload)
	mustStatus(t, status, 200)
	waitForAnyText(t, "/errors?service="+s.service, []string{s.markerToken}, 10*time.Second)
	return s
}

// ---------------------------------------------------------------------------
// TestScreenshots
// ---------------------------------------------------------------------------

func testScreenshots(t *testing.T) {
	// Seed realistic sample traffic so screenshots show populated views.
	seed(envInt("SOBS_SCREENSHOT_SEED_TOTAL", 240), envInt("SOBS_SCREENSHOT_SEED_WORKERS", 24))

	t.Run("summary", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.screenshot("summary.png", baseURL+"/")
		p.expectVisibleText("Summary")
	})

	t.Run("summary_ai_assistant", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.MustEvalOnNewDocument(initThemeScript)
		p.setViewport(1440, 900)
		p.gotoIdle(baseURL + "/")
		p.dismissTourModal()
		p.clickSel("#sobsAiBtn")
		p.MustElement("#sobsAiPanel.open")
		p.expectVisibleText("SOBS observability assistant")
		p.MustScreenshot(filepath.Join(screenshotsDir, "summary_ai_assistant.png"))
	})

	t.Run("logs", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.screenshot("logs.png", baseURL+"/logs")
		p.expectVisibleText("Logs")
	})

	t.Run("traces", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.screenshot("traces.png", baseURL+"/traces")
		p.expectVisibleText("Traces")
	})

	t.Run("traces_drilldown", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.screenshot("traces_drilldown.png", firstTraceDetailURL(t))
		p.expectVisibleText("All Traces")
	})

	t.Run("errors", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.screenshot("errors.png", baseURL+"/errors")
		p.expectVisibleText("Errors")
	})

	t.Run("rum", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.screenshot("rum.png", baseURL+"/rum")
		p.expectVisibleText("Real User Monitoring")
	})

	t.Run("ai", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.screenshot("ai.png", baseURL+"/ai")
		p.expectVisibleText("AI Transparency")
	})

	t.Run("dashboards", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		dashboardURL := createDocsDashboard(t)
		p.screenshot("dashboard.png", dashboardURL)
		p.MustElement("[id^='chart-'] canvas")
		p.expectVisibleText("Log Volume by Minute")
	})

	t.Run("query", func(t *testing.T) {
		p := newPage(t)
		defer p.close()
		p.screenshot("query.png", baseURL+"/query")
		p.expectVisibleText("Natural-Language Query")
	})

	t.Run("notifications_responsive_cards", testScreenshotNotificationsResponsiveCards)
	t.Run("errors_masking_replay_artifacts", testScreenshotErrorsMasking)
	t.Run("rum_masking_replay_artifacts", testScreenshotRumMasking)
	t.Run("tags_responsive_cards", testScreenshotTagsResponsiveCards)
}

func testScreenshotNotificationsResponsiveCards(t *testing.T) {
	p := newPage(t)
	defer p.close()
	marker := fmt.Sprintf("%d", nowMs())
	channelName := "Screenshot Channel " + marker
	ruleName := "Screenshot Rule " + marker

	createChannel := postForm(t, "/settings/notifications/channels", urlValues(
		"name", channelName,
		"channel_type", "webhook",
		"webhook_url", "http://127.0.0.1:65535/screenshot-notifications",
		"webhook_method", "POST",
		"webhook_headers", "{}",
		"webhook_body_template", "",
		"mask_output_enabled", "1",
	))
	createChannel.Body.Close()
	mustStatusIn(t, createChannel.StatusCode, 200, 302, 303)

	p.setViewport(1440, 900)
	p.gotoIdle(baseURL + "/settings/notifications")
	p.dismissTourModal()

	// Extract the channel id from the toggle form action in the DOM.
	channelAction := p.evalStr(`(name) => {
        const row = Array.from(document.querySelectorAll('tr')).find(r => r.textContent.includes(name));
        if (!row) return '';
        const form = row.querySelector("form[action*='/notifications/channels/'][action$='/toggle']");
        return form ? (form.getAttribute('action') || '') : '';
    }`, channelName)
	cm := regexp.MustCompile(`/channels/([^/]+)/toggle$`).FindStringSubmatch(channelAction)
	if cm == nil {
		t.Fatalf("could not find channel toggle action (got %q)", channelAction)
	}
	channelID := cm[1]

	createRule := postForm(t, "/settings/notifications/rules", urlValues(
		"name", ruleName,
		"logic_operator", "any",
		"severity", "warning",
		"cooldown_seconds", "0",
		"channel_ids", channelID,
		"cond_type", "signal",
		"cond_source", "logs",
		"cond_signal", "error_volume",
		"cond_service", "",
		"cond_comparator", "gt",
		"cond_threshold", "0",
		"cond_window_minutes", "15",
	))
	createRule.Body.Close()
	mustStatusIn(t, createRule.StatusCode, 200, 302, 303)

	checkResp := postForm(t, "/api/notifications/check", urlValues())
	checkResp.Body.Close()
	mustStatus(t, checkResp.StatusCode, 200)

	notificationsURL := baseURL + "/settings/notifications"
	p.screenshotAtViewport("notifications_desktop_1440.png", notificationsURL, 1440, 900)
	p.screenshotAtViewport("notifications_tablet_992.png", notificationsURL, 992, 900)
	p.screenshotAtViewport("notifications_hamburger_575.png", notificationsURL, 575, 1100)
	p.screenshotAtViewport("notifications_mobile_480.png", notificationsURL, 480, 1100)

	// Card mode active at hamburger/mobile width.
	p.setViewport(575, 1100)
	p.gotoIdle(notificationsURL)
	p.dismissTourModal()
	p.MustElement(".notification-channels-table tbody tr")
	layout := p.evalJSON(`() => {
        const styleOf = (selector) => {
            const el = document.querySelector(selector);
            return el ? window.getComputedStyle(el).display : null;
        };
        return {
            channelsHeadDisplay: styleOf('.notification-channels-table thead'),
            channelsRowDisplay: styleOf('.notification-channels-table tbody tr'),
            rulesHeadDisplay: styleOf('.notification-rules-table thead'),
            rulesRowDisplay: styleOf('.notification-rules-table tbody tr'),
            logHeadDisplay: styleOf('.notification-mobile-card-table thead'),
            logRowDisplay: styleOf('.notification-mobile-card-table tbody tr'),
        };
    }`)
	mustDisplay(t, layout, "channelsHeadDisplay", "none")
	mustDisplay(t, layout, "channelsRowDisplay", "block")
	mustDisplay(t, layout, "rulesHeadDisplay", "none")
	mustDisplay(t, layout, "rulesRowDisplay", "block")
	mustDisplay(t, layout, "logHeadDisplay", "none")
	mustDisplay(t, layout, "logRowDisplay", "block")

	// Auto Make preview table also renders as cards on mobile.
	if p.count("#autoNotifPreviewBtn") > 0 {
		btn := p.first("#autoNotifPreviewBtn")
		if !btn.MustVisible() {
			if p.count(`[data-bs-target="#autoNotifCollapse"]`) > 0 {
				p.first(`[data-bs-target="#autoNotifCollapse"]`).MustClick()
			}
			p.MustElement("#autoNotifPreviewBtn").MustWaitVisible()
		}
		p.first("#autoNotifPreviewBtn").MustClick()
		p.MustElement("#autoNotifPreviewContainer .auto-notif-preview-table")
		preview := p.evalJSON(`() => {
            const styleOf = (selector) => {
                const el = document.querySelector(selector);
                return el ? window.getComputedStyle(el).display : null;
            };
            return {
                previewHeadDisplay: styleOf('.auto-notif-preview-table thead'),
                previewRowDisplay: styleOf('.auto-notif-preview-table tbody tr'),
            };
        }`)
		mustDisplay(t, preview, "previewHeadDisplay", "none")
		mustDisplay(t, preview, "previewRowDisplay", "block")
		p.screenshotAtViewport("notifications_auto_make_mobile_575.png", notificationsURL, 575, 1300)
	}
}

func testScreenshotErrorsMasking(t *testing.T) {
	p := newPage(t)
	defer p.close()
	marker := fmt.Sprintf("%d", nowMs())
	s := seedMaskingRumError(t, marker)

	p.MustEvalOnNewDocument(initThemeScript)
	p.setViewport(1440, 900)
	p.gotoIdle(baseURL + "/errors?service=" + s.service)
	p.dismissTourModal()

	// Expand the formatted JSON body payload section.
	p.first(`div.mb-2`) // ensure DOM ready
	p.MustElementR("summary", "Formatted JSON").MustClick()
	p.MustScreenshot(filepath.Join(screenshotsDir, "errors_masking.png"))

	html := p.MustHTML()
	visible := p.MustElement("body").MustText()
	mustNotContain(t, visible, s.email)
	mustNotContain(t, visible, s.apiKey)
	mustNotContain(t, visible, s.auth)
	mustNotContain(t, visible, s.password)
	mustText(t, html, "data-rum-view-url")
	p.expectVisibleText("Errors")
}

func testScreenshotRumMasking(t *testing.T) {
	p := newPage(t)
	defer p.close()
	marker := fmt.Sprintf("%d", nowMs())
	s := seedMaskingRumError(t, marker)
	p.screenshot("rum_masking.png", baseURL+"/rum?type=error&q="+s.markerToken)

	html := p.MustHTML()
	visible := p.MustElement("body").MustText()
	tableHasMarker := p.evalBool(`(tok) => Array.from(document.querySelectorAll('table tbody'))
        .some(tb => (tb.textContent || '').includes(tok))`, s.markerToken)
	if !tableHasMarker {
		t.Errorf("marker token %q not found in any table tbody", s.markerToken)
	}
	mustNotContain(t, visible, s.email)
	mustNotContain(t, visible, s.apiKey)
	mustNotContain(t, visible, s.auth)
	mustNotContain(t, visible, s.password)
	mustText(t, html, "data-rum-view-url")
	p.expectVisibleText("Real User Monitoring")
}

func testScreenshotTagsResponsiveCards(t *testing.T) {
	p := newPage(t)
	defer p.close()
	marker := fmt.Sprintf("%d", nowMs())

	for i := 1; i <= 3; i++ {
		matchField := "service_name"
		matchValue := fmt.Sprintf("service-%d", i)
		tagValue := fmt.Sprintf("level-%d", i)
		if i == 1 {
			matchField, matchValue, tagValue = "severity", "ERROR", "critical"
		}
		resp := postForm(t, "/settings/tags", urlValues(
			"name", fmt.Sprintf("Screenshot Tag Rule %s-%d", marker, i),
			"record_types", "log",
			"match_field", matchField,
			"match_operator", "eq",
			"match_value", matchValue,
			"match_attr_key", "",
			"tag_key", "tier",
			"tag_value", tagValue,
		))
		resp.Body.Close()
		mustStatusIn(t, resp.StatusCode, 200, 302, 303)
	}

	tagsURL := baseURL + "/settings/tags"
	p.screenshotAtViewport("tags_desktop_1440.png", tagsURL, 1440, 900)
	p.screenshotAtViewport("tags_tablet_992.png", tagsURL, 992, 900)
	p.screenshotAtViewport("tags_hamburger_575.png", tagsURL, 575, 1200)
	p.screenshotAtViewport("tags_mobile_480.png", tagsURL, 480, 1200)
	p.screenshotAtViewport("tags_mobile_375.png", tagsURL, 375, 1200)

	// Mobile card mode activates at <=575px.
	p.setViewport(575, 1200)
	p.gotoIdle(tagsURL)
	p.dismissTourModal()
	p.MustElement(".tags-mobile-card-table")
	layout := p.evalJSON(`() => {
        return Array.from(document.querySelectorAll('.tags-mobile-card-table')).map((table) => {
            const thead = table.querySelector('thead');
            const row = table.querySelector('tbody tr');
            return {
                theadDisplay: thead ? window.getComputedStyle(thead).display : null,
                rowDisplay: row ? window.getComputedStyle(row).display : null,
            };
        });
    }`)
	arr := layout.Arr()
	if len(arr) == 0 {
		t.Fatalf("expected at least one tags-mobile-card-table")
	}
	for _, tl := range arr {
		if tl.Get("theadDisplay").Str() != "none" {
			t.Errorf("tags table thead should be hidden at 575px, got %q", tl.Get("theadDisplay").Str())
		}
		if tl.Get("rowDisplay").Str() != "block" {
			t.Errorf("tags table rows should be block at 575px, got %q", tl.Get("rowDisplay").Str())
		}
	}
}
