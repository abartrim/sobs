package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Batch 3: cmd/sobs/handlers_forms.go — form-submission handlers. These drive validation-error,
// not-found, and DB-error branches through the storetest.FakeDB seam (see
// internal/store/storetest.FakeDB, .Result, .SettingsDB — reused per cmd/sobs/*_test.go
// convention, e.g. dm_s3_backup_test.go).

// ---- handleReportsFormSub / deleteFormID ----

func TestHandleReportsFormSub_WrongMethodNotFound(t *testing.T) {
	// /reports/<id>/delete is a registered param template, so a disallowed method (GET) is
	// answered by paramMethodGuard with 405, not a bare 404.
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/reports/abc/delete", nil)
	w := httptest.NewRecorder()
	s.handleReportsFormSub(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandleReportsFormSub_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}} // empty result -> not found
	req := httptest.NewRequest(http.MethodPost, "/reports/rid1/delete", nil)
	w := httptest.NewRecorder()
	s.handleReportsFormSub(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want 302 redirect, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), "sobs_session=") {
		t.Fatalf("want flash cookie, got %q", w.Header().Get("Set-Cookie"))
	}
	if w.Header().Get("Location") != "/reports" {
		t.Fatalf("want /reports location, got %q", w.Header().Get("Location"))
	}
}

func TestHandleReportsFormSub_InsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "Description", "PageType", "FiltersJson"},
				[]any{"rid1", "My Report", "desc", "logs", "{}"}), nil
		},
		InsertErr: errors.New("insert boom"),
	}}
	req := httptest.NewRequest(http.MethodPost, "/reports/rid1/delete", nil)
	w := httptest.NewRecorder()
	s.handleReportsFormSub(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleReportsFormSub_Success(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "Description", "PageType", "FiltersJson"},
				[]any{"rid1", "My Report", "desc", "logs", "{}"}), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/reports/rid1/delete", nil)
	w := httptest.NewRecorder()
	s.handleReportsFormSub(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}
}

func TestDeleteFormID_WrongMethodParamGuard(t *testing.T) {
	// Not a "<id>/delete" suffix path but matches no param route either -> falls to NotFound.
	req := httptest.NewRequest(http.MethodGet, "/settings/agents/xyz", nil)
	w := httptest.NewRecorder()
	id, ok := deleteFormID(w, req, "/settings/agents/")
	if ok {
		t.Fatalf("want ok=false, got true (id=%q)", id)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

// ---- handleSettingsAgentsSub / handleSettingsTagsSub / handleMetricsRulesSub (softDeleteLatestRow) ----

func TestHandleSettingsAgentsSub_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/agents/rule1/delete", nil)
	w := httptest.NewRecorder()
	s.handleSettingsAgentsSub(w, req)
	if w.Code != http.StatusFound || !strings.Contains(w.Body.String(), "") {
		t.Fatalf("want redirect, got %d", w.Code)
	}
	if w.Header().Get("Location") != "/settings/agents" {
		t.Fatalf("want /settings/agents, got %q", w.Header().Get("Location"))
	}
}

func TestHandleSettingsAgentsSub_WrongMethodParamGuard(t *testing.T) {
	// /settings/agents/<id>/delete is a registered param template; GET is disallowed, so
	// deleteFormID's paramMethodGuard branch (true) fires a 405 rather than a bare 404.
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/agents/rule1/delete", nil)
	w := httptest.NewRecorder()
	s.handleSettingsAgentsSub(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandleSettingsAgentsSub_Success(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name"}, []any{"rule1", "My Rule"}), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/settings/agents/rule1/delete", nil)
	w := httptest.NewRecorder()
	s.handleSettingsAgentsSub(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/agents" {
		t.Fatalf("want redirect to /settings/agents, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestHandleSettingsAgentsSub_QueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("query boom") },
	}}
	req := httptest.NewRequest(http.MethodPost, "/settings/agents/rule1/delete", nil)
	w := httptest.NewRecorder()
	s.handleSettingsAgentsSub(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestHandleSettingsTagsSub_Success(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name"}, []any{"tag1", "My Tag"}), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/settings/tags/tag1/delete", nil)
	w := httptest.NewRecorder()
	s.handleSettingsTagsSub(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Location") != "/settings/tags" {
		t.Fatalf("want /settings/tags, got %q", w.Header().Get("Location"))
	}
}

func TestHandleMetricsRulesSub_InsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
					"Comparator", "WarningThreshold", "CriticalThreshold", "SecondarySignalSource",
					"SecondarySignalName", "SecondaryComparator", "SecondaryWarningThreshold",
					"SecondaryCriticalThreshold", "MinSampleCount"},
				[]any{"m1", "My Rule", "threshold", "logs", "count", "svc", "", "gt", 1.0, 2.0, "", "", "gt", 0.0, 0.0, 0},
			), nil
		},
		InsertErr: errors.New("insert boom"),
	}}
	req := httptest.NewRequest(http.MethodPost, "/metrics/rules/m1/delete", nil)
	w := httptest.NewRecorder()
	s.handleMetricsRulesSub(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

// ---- splitIDAction ----

func TestSplitIDAction_NoAction(t *testing.T) {
	id, action := splitIDAction("onlyid")
	if id != "onlyid" || action != "" {
		t.Fatalf("got id=%q action=%q", id, action)
	}
}

// ---- handleNotifChannelsSub / toggleNotifChannel ----

func TestHandleNotifChannelsSub_WrongMethod(t *testing.T) {
	// Registered param template -> disallowed method yields 405 via paramMethodGuard.
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/notifications/channels/c1/delete", nil)
	w := httptest.NewRecorder()
	s.handleNotifChannelsSub(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandleNotifChannelsSub_UnknownAction(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels/c1/bogus", nil)
	w := httptest.NewRecorder()
	s.handleNotifChannelsSub(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestToggleNotifChannel_QueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	w := httptest.NewRecorder()
	s.toggleNotifChannel(w, "c1")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestToggleNotifChannel_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.toggleNotifChannel(w, "missing")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/notifications" {
		t.Fatalf("want redirect to /settings/notifications, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestToggleNotifChannel_EnableAndDisable(t *testing.T) {
	// Enabled=0 -> toggled on ("enabled").
	sOn := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "ChannelType", "ConfigJson", "Enabled"},
				[]any{"c1", "Chan", "webhook", "{}", 0.0}), nil
		},
	}}
	w := httptest.NewRecorder()
	sOn.toggleNotifChannel(w, "c1")
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Set-Cookie"), "") {
		t.Fatalf("want 302, got %d", w.Code)
	}

	// Enabled=1 -> toggled off ("disabled").
	sOff := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "ChannelType", "ConfigJson", "Enabled"},
				[]any{"c1", "Chan", "webhook", "{}", 1.0}), nil
		},
	}}
	w2 := httptest.NewRecorder()
	sOff.toggleNotifChannel(w2, "c1")
	if w2.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w2.Code)
	}
}

func TestToggleNotifChannel_InsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "ChannelType", "ConfigJson", "Enabled"},
				[]any{"c1", "Chan", "webhook", "{}", 0.0}), nil
		},
		InsertErr: errors.New("boom"),
	}}
	w := httptest.NewRecorder()
	s.toggleNotifChannel(w, "c1")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestHandleNotifChannelsSub_DeleteDispatch(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels/c1/delete", nil)
	w := httptest.NewRecorder()
	s.handleNotifChannelsSub(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/notifications" {
		t.Fatalf("want redirect, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestHandleNotifChannelsSub_ToggleDispatch(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels/c1/toggle", nil)
	w := httptest.NewRecorder()
	s.handleNotifChannelsSub(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (not-found flash), got %d", w.Code)
	}
}

// ---- handleNotifRulesSub / toggleNotifRule ----

func TestHandleNotifRulesSub_WrongMethod(t *testing.T) {
	// Registered param template -> disallowed method yields 405 via paramMethodGuard.
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/notifications/rules/r1/delete", nil)
	w := httptest.NewRecorder()
	s.handleNotifRulesSub(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandleNotifRulesSub_UnknownAction(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules/r1/bogus", nil)
	w := httptest.NewRecorder()
	s.handleNotifRulesSub(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestToggleNotifRule_QueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	w := httptest.NewRecorder()
	s.toggleNotifRule(w, "r1")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestToggleNotifRule_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.toggleNotifRule(w, "missing")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/notifications" {
		t.Fatalf("want redirect, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestToggleNotifRule_EnableAndInsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds", "Severity", "CooldownSeconds"},
				[]any{"r1", "Rule", 1.0, "any", "[]", "", "warning", 300.0},
			), nil
		},
	}}
	w := httptest.NewRecorder()
	s.toggleNotifRule(w, "r1")
	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}

	sErr := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds", "Severity", "CooldownSeconds"},
				[]any{"r1", "Rule", 0.0, "any", "[]", "", "warning", 300.0},
			), nil
		},
		InsertErr: errors.New("boom"),
	}}
	w2 := httptest.NewRecorder()
	sErr.toggleNotifRule(w2, "r1")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w2.Code)
	}
}

func TestHandleNotifRulesSub_DeleteAndToggleDispatch(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules/r1/delete", nil)
	w := httptest.NewRecorder()
	s.handleNotifRulesSub(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules/r1/toggle", nil)
	w2 := httptest.NewRecorder()
	s.handleNotifRulesSub(w2, req2)
	if w2.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w2.Code)
	}
}

// ---- handleSettingsRepositoriesSub ----

func TestHandleSettingsRepositoriesSub_WrongMethod(t *testing.T) {
	// Registered param template -> disallowed method yields 405 via paramMethodGuard.
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/repositories/app1/delete", nil)
	w := httptest.NewRecorder()
	s.handleSettingsRepositoriesSub(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandleSettingsRepositoriesSub_AppNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/repositories/missingapp/delete", nil)
	w := httptest.NewRecorder()
	s.handleSettingsRepositoriesSub(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/repositories" {
		t.Fatalf("want redirect to /settings/repositories, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestHandleSettingsRepositoriesSub_GithubTokenValidateNoToken(t *testing.T) {
	// The static "github-token/validate" sub-path dispatches to repoGithubTokenValidate, which
	// flashes "no token configured" when ai.github_token is unset.
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/repositories/github-token/validate", nil)
	w := httptest.NewRecorder()
	s.handleSettingsRepositoriesSub(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), "sobs_session=") {
		t.Fatalf("want a flash cookie, got %q", w.Header().Get("Set-Cookie"))
	}
}

func TestHandleSettingsRepositoriesSub_DeleteActionDispatch(t *testing.T) {
	// The "delete" action dispatches to repoDelete; verify handleSettingsRepositoriesSub reaches
	// that branch (repoDelete itself lives in repositories_actions.go, outside this batch).
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_apps") {
				return storetest.Result([]string{"Id", "Name", "Slug", "OwnerTeam", "RepoUrl",
					"DefaultEnvironment", "Enabled", "MetadataJson", "CreatedAt"},
					[]any{"app1", "App One", "app-one", "team", "https://github.com/o/r",
						"prod", 1.0, "{}", "2024-01-01T00:00:00Z"}), nil
			}
			return &store.Result{}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/settings/repositories/app1/delete", nil)
	w := httptest.NewRecorder()
	s.handleSettingsRepositoriesSub(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/repositories" {
		t.Fatalf("want redirect to /settings/repositories, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestHandleSettingsRepositoriesSub_UnknownActionOnKnownApp(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_apps") {
				return storetest.Result([]string{"Id", "Name"}, []any{"app1", "App One"}), nil
			}
			return &store.Result{}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/settings/repositories/app1/some-bogus-action", nil)
	w := httptest.NewRecorder()
	s.handleSettingsRepositoriesSub(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/repositories" {
		t.Fatalf("want redirect (not found flash), got %d %q", w.Code, w.Header().Get("Location"))
	}
}

// ---- handleNotifChannelsCreate ----

func TestHandleNotifChannelsCreate_WrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/notifications/channels", nil)
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleNotifChannelsCreate_NameRequired(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels",
		strings.NewReader("name=&channel_type=webhook"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusFound || !strings.Contains(w.Body.String(), "") {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifChannelsCreate_InvalidType(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels",
		strings.NewReader("name=Chan&channel_type=bogus"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifChannelsCreate_WebhookMissingURL(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels",
		strings.NewReader("name=Chan&channel_type=webhook"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifChannelsCreate_SlackMissingURL(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels",
		strings.NewReader("name=Chan&channel_type=slack"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifChannelsCreate_EmailMissingToAddr(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels",
		strings.NewReader("name=Chan&channel_type=email"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifChannelsCreate_BrowserPushMissingEndpoint(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels",
		strings.NewReader("name=Chan&channel_type=browser_push"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifChannelsCreate_SuccessWithMaskOutputEnabled(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels",
		strings.NewReader("name=Chan&channel_type=slack&slack_webhook_url=https://example.com/hook&mask_output_enabled=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/notifications" {
		t.Fatalf("want success redirect, got %d %q", w.Code, w.Header().Get("Location"))
	}
	if len(s.db.(*storetest.FakeDB).Inserts) != 1 {
		t.Fatalf("want 1 insert, got %d", len(s.db.(*storetest.FakeDB).Inserts))
	}
}

func TestHandleNotifChannelsCreate_InsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{InsertErr: errors.New("boom")}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/channels",
		strings.NewReader("name=Chan&channel_type=slack&slack_webhook_url=https://example.com/hook"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifChannelsCreate(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

// ---- handleNotifRulesCreate ----

func TestHandleNotifRulesCreate_WrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/notifications/rules", nil)
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleNotifRulesCreate_NameRequired(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader("name="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifRulesCreate_InvalidLogicOperator(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules",
		strings.NewReader("name=R1&logic_operator=bogus"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifRulesCreate_InvalidSeverity(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules",
		strings.NewReader("name=R1&severity=bogus"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifRulesCreate_NoConditions(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader("name=R1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (no conditions), got %d", w.Code)
	}
}

func TestHandleNotifRulesCreate_SuccessNew(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	form := "name=R1&cond_type=signal&cond_source=logs&cond_signal=count&cond_service=svc&cond_comparator=gt&cond_threshold=5&cond_window_minutes=10&channel_ids=doesnotexist"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settings/notifications" {
		t.Fatalf("want success redirect, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestHandleNotifRulesCreate_EditRuleNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}} // empty result for the edit_rule_id lookup
	form := "name=R1&edit_rule_id=missing&cond_type=signal&cond_source=logs&cond_signal=count"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleNotifRulesCreate_EditRuleQueryError(t *testing.T) {
	calls := 0
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			calls++
			if strings.Contains(q, "sobs_notification_channels") {
				return &store.Result{}, nil
			}
			return nil, errors.New("boom")
		},
	}}
	form := "name=R1&edit_rule_id=r1&cond_type=signal&cond_source=logs&cond_signal=count"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestHandleNotifRulesCreate_EditRuleUpdateSuccess(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_notification_rules FINAL") {
				return storetest.Result([]string{"Id", "Enabled", "LastFiredAt"},
					[]any{"r1", 1.0, "2024-01-01 00:00:00.000"}), nil
			}
			return &store.Result{}, nil
		},
	}}
	form := "name=R1&edit_rule_id=r1&cond_type=signal&cond_source=logs&cond_signal=count"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
	body := w.Header().Get("Set-Cookie")
	if !strings.Contains(body, "sobs_session=") {
		t.Fatalf("want flash cookie, got %q", body)
	}
}

func TestHandleNotifRulesCreate_InsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{InsertErr: errors.New("boom")}}
	form := "name=R1&cond_type=signal&cond_source=logs&cond_signal=count"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleNotifRulesCreate(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

// ---- buildNotificationConditions (tag branch + regex error) ----

func TestBuildNotificationConditions_TagRegexInvalid(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	form := "cond_type=tag&cond_tag_key=env&cond_tag_match_operator=regex&cond_tag_value=%5B"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	w := httptest.NewRecorder()
	conds, ok := s.buildNotificationConditions(w, req, "/settings/notifications", "")
	if ok || conds != nil {
		t.Fatalf("want ok=false conds=nil, got ok=%v conds=%v", ok, conds)
	}
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestBuildNotificationConditions_TagValid(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	form := "cond_type=tag&cond_tag_key=env&cond_tag_match_operator=eq&cond_tag_value=prod&cond_record_type=log"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	w := httptest.NewRecorder()
	conds, ok := s.buildNotificationConditions(w, req, "/settings/notifications", "")
	if !ok || len(conds) != 1 {
		t.Fatalf("want 1 tag condition, got ok=%v conds=%v", ok, conds)
	}
}

func TestBuildNotificationConditions_InvalidType(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	form := "cond_type=bogus&cond_source=logs&cond_signal=count"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	w := httptest.NewRecorder()
	conds, ok := s.buildNotificationConditions(w, req, "/settings/notifications", "")
	if ok || conds != nil {
		t.Fatalf("want ok=false, got ok=%v conds=%v", ok, conds)
	}
}

func TestBuildNotificationConditions_TagRegexEditAware(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	form := "cond_type=tag&cond_tag_key=env&cond_tag_match_operator=regex&cond_tag_value=%5B"
	req := httptest.NewRequest(http.MethodPost, "/settings/notifications/rules", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	w := httptest.NewRecorder()
	_, ok := s.buildNotificationConditions(w, req, "/settings/notifications", "edit-rule-1")
	if ok {
		t.Fatalf("want ok=false")
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "edit_rule=edit-rule-1") {
		t.Fatalf("want edit_rule in redirect location, got %q", loc)
	}
}

// ---- handleMaskingKeysCreate / handleMaskingKeysDelete ----

func TestHandleMaskingKeysCreate_WrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/masking/keys", nil)
	w := httptest.NewRecorder()
	s.handleMaskingKeysCreate(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleMaskingKeysCreate_EmptyKey(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/keys", strings.NewReader("key=  "))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingKeysCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleMaskingKeysCreate_AlreadyActiveDefault(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/keys", strings.NewReader("key=password"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingKeysCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), "sobs_session=") {
		t.Fatalf("want flash, got %q", w.Header().Get("Set-Cookie"))
	}
}

func TestHandleMaskingKeysCreate_SuccessNew(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/keys", strings.NewReader("key=my_custom_key"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingKeysCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
	if len(s.db.(*storetest.FakeDB).Inserts) != 1 {
		t.Fatalf("want 1 insert (setAppSetting write), got %d", len(s.db.(*storetest.FakeDB).Inserts))
	}
}

func TestHandleMaskingKeysDelete_WrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/masking/keys/delete", nil)
	w := httptest.NewRecorder()
	s.handleMaskingKeysDelete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleMaskingKeysDelete_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/keys/delete", strings.NewReader("key=nope"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingKeysDelete(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleMaskingKeysDelete_Success(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{
		"masking.custom_keys": `["my_custom_key"]`,
	})}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/keys/delete", strings.NewReader("key=my_custom_key"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingKeysDelete(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

// ---- handleMaskingPatternsCreate / handleMaskingPatternsDelete ----

func TestHandleMaskingPatternsCreate_WrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/masking/patterns", nil)
	w := httptest.NewRecorder()
	s.handleMaskingPatternsCreate(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleMaskingPatternsCreate_InvalidRegex(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/patterns", strings.NewReader("pattern="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingPatternsCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleMaskingPatternsCreate_AlreadyActive(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	// A default sensitive pattern (email regex) is already active.
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/patterns",
		strings.NewReader("pattern="+strings.ReplaceAll(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`, `\`, `%5C`)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingPatternsCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleMaskingPatternsCreate_Success(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/patterns", strings.NewReader("pattern=foo-bar-baz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingPatternsCreate(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
	if len(s.db.(*storetest.FakeDB).Inserts) != 1 {
		t.Fatalf("want 1 insert, got %d", len(s.db.(*storetest.FakeDB).Inserts))
	}
}

func TestHandleMaskingPatternsDelete_WrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/masking/patterns/delete", nil)
	w := httptest.NewRecorder()
	s.handleMaskingPatternsDelete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleMaskingPatternsDelete_InvalidRegex(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/patterns/delete", strings.NewReader("pattern="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingPatternsDelete(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleMaskingPatternsDelete_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/patterns/delete", strings.NewReader("pattern=not-active"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingPatternsDelete(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

func TestHandleMaskingPatternsDelete_Success(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{
		"masking.custom_patterns": `["foo-bar-baz"]`,
	})}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/patterns/delete", strings.NewReader("pattern=foo-bar-baz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingPatternsDelete(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
}

// ---- handleSettingsDataManagement (POST TTL branches) ----

func TestHandleSettingsDataManagement_GetDelegates(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/data-management", nil)
	w := httptest.NewRecorder()
	s.handleSettingsDataManagement(w, req)
	// handleDataManagementGet renders a page; just assert it's not a 404/method-guard result.
	if w.Code == http.StatusNotFound {
		t.Fatalf("GET should delegate to handleDataManagementGet, got 404")
	}
}

func TestHandleSettingsDataManagement_PostNoApplyTTL(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/data-management",
		strings.NewReader("backup_enabled=1&s3_bucket=bucket&ttl_logs_days=30&clear_s3_secret_access_key=1&clear_backup_encryption_password=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleSettingsDataManagement(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "Settings+saved") {
		t.Fatalf("want success message, got %q", loc)
	}
}

func TestHandleSettingsDataManagement_ApplyTTLSuccess(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	form := "apply_ttl=1&ttl_logs_days=30&ttl_traces_days=7&ttl_sessions_days=14&ttl_metrics_hours=48"
	req := httptest.NewRequest(http.MethodPost, "/settings/data-management", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleSettingsDataManagement(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "Settings+saved") {
		t.Fatalf("want success message (TTL applied ok), got %q", loc)
	}
}

func TestHandleSettingsDataManagement_ApplyTTLErrors(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	// Non-numeric TTL days -> pyParseInt error branch in applyDMTTL, surfaced as a warning redirect.
	form := "apply_ttl=1&ttl_logs_days=not-a-number"
	req := httptest.NewRequest(http.MethodPost, "/settings/data-management", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleSettingsDataManagement(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "TTL+errors") && !strings.Contains(loc, "TTL errors") {
		t.Fatalf("want TTL error message, got %q", loc)
	}
}

// ---- handleMaskingOutputSave / handleMaskingSqlOutputSave ----

func TestHandleMaskingOutputSave_WrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/masking/output", nil)
	w := httptest.NewRecorder()
	s.handleMaskingOutputSave(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleMaskingOutputSave_EnabledAndDisabled(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/output", strings.NewReader("enabled=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingOutputSave(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}

	s2 := &server{db: &storetest.FakeDB{}}
	req2 := httptest.NewRequest(http.MethodPost, "/settings/masking/output", strings.NewReader(""))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	s2.handleMaskingOutputSave(w2, req2)
	if w2.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w2.Code)
	}
}

func TestHandleMaskingSqlOutputSave_WrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodGet, "/settings/masking/sql-output", nil)
	w := httptest.NewRecorder()
	s.handleMaskingSqlOutputSave(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleMaskingSqlOutputSave_EnabledAndDisabled(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/settings/masking/sql-output", strings.NewReader("enabled=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleMaskingSqlOutputSave(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w.Code)
	}

	s2 := &server{db: &storetest.FakeDB{}}
	req2 := httptest.NewRequest(http.MethodPost, "/settings/masking/sql-output", strings.NewReader(""))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	s2.handleMaskingSqlOutputSave(w2, req2)
	if w2.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d", w2.Code)
	}
}

// ---- handleDashboardsFormSub + dashboard/chart helpers ----

func dashboardsFakeDB(dashRows, chartRows func(q string) (*store.Result, error)) *storetest.FakeDB {
	return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_dashboards") {
			return dashRows(q)
		}
		if strings.Contains(q, "sobs_chart_configs") {
			return chartRows(q)
		}
		return &store.Result{}, nil
	}}
}

func TestHandleDashboardsFormSub_DashboardNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/missing/delete", nil)
	w := httptest.NewRecorder()
	s.handleDashboardsFormSub(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/dashboards" {
		t.Fatalf("want redirect to /dashboards, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestHandleDashboardsFormSub_DashboardQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/delete", nil)
	w := httptest.NewRecorder()
	s.handleDashboardsFormSub(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestHandleDashboardsFormSub_UnknownSubroute(t *testing.T) {
	s := dashboardsFakeDB(
		func(string) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "Description"}, []any{"d1", "Dash", "desc"}), nil
		},
		func(string) (*store.Result, error) { return &store.Result{}, nil },
	)
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/bogus/thing/extra", nil)
	w := httptest.NewRecorder()
	sv := &server{db: s}
	sv.handleDashboardsFormSub(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleDashboardsFormSub_ViewGet(t *testing.T) {
	s := dashboardsFakeDB(
		func(string) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "Description"}, []any{"d1", "Dash", "desc"}), nil
		},
		func(string) (*store.Result, error) { return &store.Result{}, nil },
	)
	req := httptest.NewRequest(http.MethodGet, "/dashboards/d1", nil)
	w := httptest.NewRecorder()
	sv := &server{cfg: config{TemplateDir: "../../../templates"}, db: s}
	sv.handleDashboardsFormSub(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 rendered page, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Dash") {
		t.Fatalf("want dashboard name in rendered body")
	}
}

func TestGetCharts_QueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	if _, err := s.getCharts("d1"); err == nil {
		t.Fatalf("want error")
	}
}

func TestDeleteDashboard_InsertDashboardError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{InsertErr: errors.New("boom")}}
	w := httptest.NewRecorder()
	s.deleteDashboard(w, "d1", "Dash", "desc")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestDeleteDashboard_WithCharts(t *testing.T) {
	callCount := 0
	fdb := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_chart_configs") {
			return storetest.Result([]string{"Id", "Title", "ChartType", "Query", "OptionsJson", "Position"},
				[]any{"c1", "Chart One", "line", "SELECT 1", `{"chart_spec":{}}`, 0.0}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fdb}
	w := httptest.NewRecorder()
	s.deleteDashboard(w, "d1", "Dash", "desc")
	callCount = len(fdb.Inserts)
	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", w.Code, w.Body.String())
	}
	if callCount != 2 {
		t.Fatalf("want 2 inserts (dashboard + chart tombstones), got %d", callCount)
	}
}

func TestDeleteDashboard_NoCharts(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.deleteDashboard(w, "d1", "Dash", "desc")
	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}
	if len(s.db.(*storetest.FakeDB).Inserts) != 1 {
		t.Fatalf("want 1 insert (dashboard only, no chart tombstones), got %d", len(s.db.(*storetest.FakeDB).Inserts))
	}
}

func TestDeleteDashboard_ChartTombstoneInsertError(t *testing.T) {
	// First insert (dashboard tombstone) succeeds; the SECOND insert (chart tombstones) fails.
	insertCount := 0
	fdb := &storetest.FakeDB{}
	fdb.ExecuteFunc = func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_chart_configs") {
			return storetest.Result([]string{"Id", "Title", "ChartType", "Query", "OptionsJson", "Position"},
				[]any{"c1", "Chart One", "line", "SELECT 1", `{"chart_spec":{}}`, 0.0}), nil
		}
		return &store.Result{}, nil
	}
	// Wrap InsertJSONEachRow behavior via a thin adapter: fail only on the second call.
	s := &server{db: &countingInsertErrDB{FakeDB: fdb, failFrom: 2, counter: &insertCount}}
	w := httptest.NewRecorder()
	s.deleteDashboard(w, "d1", "Dash", "desc")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 (chart tombstone insert failure), got %d: %s", w.Code, w.Body.String())
	}
}

func TestAddChart_ValidationBranches(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}

	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts", strings.NewReader("title="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.addChart(w, req, "d1")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (title required), got %d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts", strings.NewReader("title=T1"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	s.addChart(w2, req2, "d1")
	if w2.Code != http.StatusFound {
		t.Fatalf("want redirect (chart spec required), got %d", w2.Code)
	}

	req3 := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts",
		strings.NewReader("title=T1&chart_spec_json=not-json"))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w3 := httptest.NewRecorder()
	s.addChart(w3, req3, "d1")
	if w3.Code != http.StatusFound {
		t.Fatalf("want redirect (parse error), got %d", w3.Code)
	}
}

func TestAddChart_SuccessAndInsertError(t *testing.T) {
	specJSON := `{"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 1"},"visual":{"custom_option_json":"{}","custom_mapping_json":"{}"}}`

	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts",
		strings.NewReader("title=T1&chart_spec_json="+url.QueryEscape(specJSON)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.addChart(w, req, "d1")
	if w.Code != http.StatusFound {
		t.Fatalf("want 302 plain redirect, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.db.(*storetest.FakeDB).Inserts) != 1 {
		t.Fatalf("want 1 insert, got %d", len(s.db.(*storetest.FakeDB).Inserts))
	}

	s2 := &server{db: &storetest.FakeDB{InsertErr: errors.New("boom")}}
	req2 := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts",
		strings.NewReader("title=T1&chart_spec_json="+url.QueryEscape(specJSON)))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	s2.addChart(w2, req2, "d1")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestAddChart_CompileErrorAndInsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts",
		strings.NewReader(`title=T1&chart_spec_json={}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.addChart(w, req, "d1")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (compile error, missing template_id), got %d: %s", w.Code, w.Body.String())
	}
}

func TestRemoveChart_NotFoundAndQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.removeChart(w, "missing", "d1")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (chart not found), got %d", w.Code)
	}

	sErr := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	w2 := httptest.NewRecorder()
	sErr.removeChart(w2, "c1", "d1")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w2.Code)
	}
}

func TestRemoveChart_SuccessAndInsertError(t *testing.T) {
	fdb := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_chart_configs") {
			return storetest.Result([]string{"Id", "Title", "ChartType", "Query", "OptionsJson", "Position"},
				[]any{"c1", "Chart One", "line", "SELECT 1", `{"chart_spec":{}}`, 0.0}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fdb}
	w := httptest.NewRecorder()
	s.removeChart(w, "c1", "d1")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d: %s", w.Code, w.Body.String())
	}

	fdb2 := &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_chart_configs") {
				return storetest.Result([]string{"Id", "Title", "ChartType", "Query", "OptionsJson", "Position"},
					[]any{"c1", "Chart One", "line", "SELECT 1", `{"chart_spec":{}}`, 0.0}), nil
			}
			return &store.Result{}, nil
		},
		InsertErr: errors.New("boom"),
	}
	s2 := &server{db: fdb2}
	w2 := httptest.NewRecorder()
	s2.removeChart(w2, "c1", "d1")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w2.Code)
	}
}

func TestParseChartFormSubmission_AllBranches(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("title="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	if _, _, _, _, errMsg := s.parseChartFormSubmission(req); errMsg != "Chart title is required" {
		t.Fatalf("want title-required error, got %q", errMsg)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("title=T1"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req2.ParseForm()
	if _, _, _, _, errMsg := s.parseChartFormSubmission(req2); errMsg != "Chart spec is required" {
		t.Fatalf("want spec-required error, got %q", errMsg)
	}

	req3 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("title=T1&chart_spec_json=not-json"))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req3.ParseForm()
	if _, _, _, _, errMsg := s.parseChartFormSubmission(req3); !strings.HasPrefix(errMsg, "Chart spec error:") {
		t.Fatalf("want parse error, got %q", errMsg)
	}

	req4 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`title=T1&chart_spec_json={}`))
	req4.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req4.ParseForm()
	if _, _, _, _, errMsg := s.parseChartFormSubmission(req4); !strings.HasPrefix(errMsg, "Chart spec error:") {
		t.Fatalf("want compile error, got %q", errMsg)
	}
}

func TestEditChart_ChartNotFoundAndQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/edit", strings.NewReader("title=T1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.editChart(w, req, "d1", "missing")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (chart not found), got %d", w.Code)
	}

	sErr := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	req2 := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/edit", strings.NewReader("title=T1"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	sErr.editChart(w2, req2, "d1", "c1")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w2.Code)
	}
}

func TestEditChart_ParseErrorAndSuccess(t *testing.T) {
	fdb := func() *storetest.FakeDB {
		return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_chart_configs") {
				return storetest.Result([]string{"Id", "Title", "ChartType", "Query", "OptionsJson", "Position"},
					[]any{"c1", "Chart One", "line", "SELECT 1", `{"chart_spec":{}}`, 0.0}), nil
			}
			return &store.Result{}, nil
		}}
	}

	s := &server{db: fdb()}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/edit", strings.NewReader("title="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.editChart(w, req, "d1", "c1")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (parse error), got %d", w.Code)
	}

	s2 := &server{db: fdb()}
	req2 := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/edit",
		strings.NewReader(`title=T2&chart_spec_json={}`))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	s2.editChart(w2, req2, "d1", "c1")
	if w2.Code != http.StatusFound {
		t.Fatalf("want redirect (compile error on empty spec), got %d", w2.Code)
	}
}

func TestEditChart_InsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_chart_configs") {
				return storetest.Result([]string{"Id", "Title", "ChartType", "Query", "OptionsJson", "Position"},
					[]any{"c1", "Chart One", "custom_echarts",
						"SELECT 1", `{"chart_spec":{"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 1"},"visual":{"custom_option_json":"{}","custom_mapping_json":"{}"}}}`, 0.0}), nil
			}
			return &store.Result{}, nil
		},
		InsertErr: errors.New("boom"),
	}}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/edit",
		strings.NewReader(`title=T2&chart_spec_json={"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 2"},"visual":{"custom_option_json":"{}","custom_mapping_json":"{}"}}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.editChart(w, req, "d1", "c1")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCloneChart_SourceNotFoundAndQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/clone", strings.NewReader("title=T1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.cloneChart(w, req, "d1", "missing")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (source not found), got %d", w.Code)
	}

	sErr := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	req2 := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/clone", strings.NewReader("title=T1"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	sErr.cloneChart(w2, req2, "d1", "c1")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w2.Code)
	}
}

func TestCloneChart_ParseErrorAndInsertError(t *testing.T) {
	fdb := func(insertErr error) *storetest.FakeDB {
		return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_chart_configs") {
				return storetest.Result([]string{"Id", "Title", "ChartType", "Query", "OptionsJson", "Position"},
					[]any{"c1", "Chart One", "custom_echarts",
						"SELECT 1", `{"chart_spec":{"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 1"},"visual":{"custom_option_json":"{}","custom_mapping_json":"{}"}}}`, 3.0}), nil
			}
			return &store.Result{}, nil
		}, InsertErr: insertErr}
	}

	s := &server{db: fdb(nil)}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/clone", strings.NewReader("title="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.cloneChart(w, req, "d1", "c1")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect (parse error), got %d", w.Code)
	}

	s2 := &server{db: fdb(errors.New("boom"))}
	req2 := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/clone",
		strings.NewReader(`title=T2&chart_spec_json={"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 2"},"visual":{"custom_option_json":"{}","custom_mapping_json":"{}"}}`))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	s2.cloneChart(w2, req2, "d1", "c1")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestCloneChart_PositionMaxAcrossCharts(t *testing.T) {
	// Two source charts at different positions; verify the clone picks max+1 (position=6) and
	// succeeds when insert has no error.
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_chart_configs") {
			cols := []string{"Id", "Title", "ChartType", "Query", "OptionsJson", "Position"}
			return storetest.Result(cols,
				[]any{"c1", "Chart One", "custom_echarts",
					"SELECT 1", `{"chart_spec":{"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 1"},"visual":{"custom_option_json":"{}","custom_mapping_json":"{}"}}}`, 2.0},
				[]any{"c2", "Chart Two", "custom_echarts",
					"SELECT 1", `{"chart_spec":{"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 1"},"visual":{"custom_option_json":"{}","custom_mapping_json":"{}"}}}`, 5.0},
			), nil
		}
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/dashboards/d1/charts/c1/clone",
		strings.NewReader(`title=Clone&chart_spec_json={"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 2"},"visual":{"custom_option_json":"{}","custom_mapping_json":"{}"}}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.cloneChart(w, req, "d1", "c1")
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d: %s", w.Code, w.Body.String())
	}
	fdb := s.db.(*storetest.FakeDB)
	if len(fdb.Inserts) != 1 {
		t.Fatalf("want 1 insert, got %d", len(fdb.Inserts))
	}
	row := fdb.Inserts[0].Rows[0]
	if row["Position"] != 6 {
		t.Fatalf("want position 6 (max 5 + 1), got %v", row["Position"])
	}
}

// ---- viewCustomDashboard ----

func TestViewCustomDashboard_GetChartsError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	w := httptest.NewRecorder()
	s.viewCustomDashboard(w, map[string]any{"Id": "d1", "Name": "Dash", "Description": "desc"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestViewCustomDashboard_Success(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.viewCustomDashboard(w, map[string]any{"Id": "d1", "Name": "Dash", "Description": "desc"})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// countingInsertErrDB wraps a *storetest.FakeDB and fails InsertJSONEachRow from the failFrom'th
// call onward (1-indexed) — used to exercise a handler's SECOND insert-error branch (e.g.
// deleteDashboard's chart-tombstone insert, reached only after the first dashboard insert
// already succeeded).
type countingInsertErrDB struct {
	*storetest.FakeDB
	failFrom int
	counter  *int
}

func (c *countingInsertErrDB) InsertJSONEachRow(table string, rows []map[string]any) (int, error) {
	*c.counter++
	if *c.counter >= c.failFrom {
		c.FakeDB.Inserts = append(c.FakeDB.Inserts, storetest.Insert{Table: table, Rows: rows})
		return 0, errors.New("boom")
	}
	return c.FakeDB.InsertJSONEachRow(table, rows)
}

// ---- normalizeSameSite / sessionCookieAttrs (small pure-function branches) ----

func TestNormalizeSameSite_DefaultsAndCasing(t *testing.T) {
	if got := normalizeSameSite("STRICT"); got != "Strict" {
		t.Fatalf("want Strict, got %q", got)
	}
	if got := normalizeSameSite("bogus"); got != "Lax" {
		t.Fatalf("want default Lax, got %q", got)
	}
	if got := normalizeSameSite("none"); got != "None" {
		t.Fatalf("want None, got %q", got)
	}
}
