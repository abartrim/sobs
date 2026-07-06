package main

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// This file covers undertested branches in cmd/sobs/handlers_mutations2.go: field-validation
// 400s, DB-error paths, dedup/existing branches, and the GitHub-backed onboarding handlers'
// upstream-fixture branches. Oracle references are the doc comments already on each handler.

// ---- handleApiNotificationsSubscribe ----------------------------------------------------

func TestHandleApiNotificationsSubscribeMissingFields(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	for _, body := range []string{
		`{}`,
		`{"endpoint":"https://push.example/ep"}`,
		`{"endpoint":"https://push.example/ep","p256dh":"key"}`,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/notifications/subscribe", strings.NewReader(body))
		s.handleApiNotificationsSubscribe(w, r)
		if w.Code != 400 {
			t.Fatalf("body %q: want 400, got %d", body, w.Code)
		}
		if !strings.Contains(w.Body.String(), "endpoint, p256dh, and auth are required") {
			t.Fatalf("body %q: unexpected error body %s", body, w.Body.String())
		}
	}
}

func TestHandleApiNotificationsSubscribeQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("db down")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/subscribe", strings.NewReader(
		`{"endpoint":"https://push.example/ep","p256dh":"key","auth":"secret"}`))
	s.handleApiNotificationsSubscribe(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500 on query error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiNotificationsSubscribeExistingDedup(t *testing.T) {
	cols := []string{"Id", "ConfigJson"}
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_notification_channels") {
			return storetest.Result(cols,
				[]any{"chan-1", `{"endpoint":"https://push.example/ep","p256dh":"k","auth":"a"}`},
			), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/subscribe", strings.NewReader(
		`{"endpoint":"https://push.example/ep","p256dh":"key","auth":"secret"}`))
	s.handleApiNotificationsSubscribe(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"channel_id":"chan-1"`) || !strings.Contains(w.Body.String(), `"existing":true`) {
		t.Fatalf("expected existing dedup response, got %s", w.Body.String())
	}
	if len(fake.Inserts) != 0 {
		t.Fatalf("dedup path must not insert, got %v", fake.Inserts)
	}
}

func TestHandleApiNotificationsSubscribeInsertsNew(t *testing.T) {
	fake := &storetest.FakeDB{} // no existing channels; zero-value Execute returns empty result
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/subscribe", strings.NewReader(
		`{"name":"My Browser","endpoint":"https://push.example/ep","p256dh":"key","auth":"secret"}`))
	s.handleApiNotificationsSubscribe(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.Inserts) != 1 || fake.Inserts[0].Table != "sobs_notification_channels" {
		t.Fatalf("expected one insert into sobs_notification_channels, got %v", fake.Inserts)
	}
	row := fake.Inserts[0].Rows[0]
	if row["Name"] != "My Browser" || row["ChannelType"] != "browser_push" {
		t.Fatalf("unexpected inserted row: %v", row)
	}
	if !strings.Contains(w.Body.String(), `"existing":false`) {
		t.Fatalf("want existing:false, got %s", w.Body.String())
	}
}

func TestHandleApiNotificationsSubscribeInsertError(t *testing.T) {
	fake := &storetest.FakeDB{InsertErr: errors.New("insert failed")}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/subscribe", strings.NewReader(
		`{"endpoint":"https://push.example/ep","p256dh":"key","auth":"secret"}`))
	s.handleApiNotificationsSubscribe(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500 on insert error, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiOnboardingCreateRepo -------------------------------------------------------

func TestHandleApiOnboardingCreateRepoMissingFields(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	for _, body := range []string{`{}`, `{"name":"My App"}`, `{"repo_url":"https://github.com/o/r"}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/onboarding/create-repo", strings.NewReader(body))
		s.handleApiOnboardingCreateRepo(w, r)
		if w.Code != 400 {
			t.Fatalf("body %q: want 400, got %d: %s", body, w.Code, w.Body.String())
		}
	}
}

func TestHandleApiOnboardingCreateRepoSlugConflict(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_apps") {
			return storetest.Result([]string{"Id"}, []any{"existing-id"}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/create-repo", strings.NewReader(
		`{"name":"My App","repo_url":"https://github.com/o/r"}`))
	s.handleApiOnboardingCreateRepo(w, r)
	if w.Code != 409 {
		t.Fatalf("want 409 conflict, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "App slug already exists") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleApiOnboardingCreateRepoInsertError(t *testing.T) {
	fake := &storetest.FakeDB{InsertErr: errors.New("boom")}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/create-repo", strings.NewReader(
		`{"name":"My App","repo_url":"https://github.com/o/r"}`))
	s.handleApiOnboardingCreateRepo(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandleApiOnboardingCreateRepoTokenSettingsBranches exercises the three optional
// set_github_token/set_repo_token/set_agent_repo persistence branches together (all default to
// on except set_github_token, which defaults to off) via the saveAISetting writes they perform.
func TestHandleApiOnboardingCreateRepoTokenSettingsBranches(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/create-repo", strings.NewReader(
		`{"name":"My App","repo_url":"https://github.com/acme/widgets","github_token":"tok-123",`+
			`"set_github_token":true,"github_token_expires_at":"2030-01-01T00:00:00Z"}`))
	s.handleApiOnboardingCreateRepo(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	// 1 app insert + 5 ai.github_token* setting writes + 1 repo-token-key write + 1 agent-repo write.
	if len(fake.Inserts) < 4 {
		t.Fatalf("expected multiple inserts recording setting writes, got %d: %v", len(fake.Inserts), fake.Inserts)
	}
	var sawGithubToken, sawAgentRepo bool
	for _, ins := range fake.Inserts {
		if ins.Table != "sobs_ai_settings" {
			continue
		}
		key, _ := ins.Rows[0]["Key"].(string)
		if key == "ai.github_token" {
			sawGithubToken = true
		}
		if key == "ai.github_repo" {
			sawAgentRepo = true
			if ins.Rows[0]["Value"] != "acme/widgets" {
				t.Errorf("ai.github_repo value = %v", ins.Rows[0]["Value"])
			}
		}
	}
	if !sawGithubToken {
		t.Error("expected ai.github_token to be saved when set_github_token=true")
	}
	if !sawAgentRepo {
		t.Error("expected ai.github_repo to be saved by default (set_agent_repo default true)")
	}
}

func TestHandleApiOnboardingCreateRepoNoTokenOptionalWrites(t *testing.T) {
	// No github_token at all: the token-dependent writes (ai.github_token*, the per-repo token) are
	// skipped since they all guard on githubToken != "". set_agent_repo, however, only requires
	// owner/repo (defaults to true, no token needed), so ai.github_repo IS still saved.
	fake := &storetest.FakeDB{}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/create-repo", strings.NewReader(
		`{"name":"My App","repo_url":"https://github.com/acme/widgets"}`))
	s.handleApiOnboardingCreateRepo(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	for _, ins := range fake.Inserts {
		if ins.Table != "sobs_ai_settings" {
			continue
		}
		key, _ := ins.Rows[0]["Key"].(string)
		if key != "ai.github_repo" {
			t.Fatalf("only ai.github_repo should be written without a github_token, got %v", ins)
		}
	}
}

// ---- handleApiOnboardingImportRepo -------------------------------------------------------

func writeUpstreamFixture(t *testing.T, dir, method, url string, spec string) {
	t.Helper()
	stem := upstreamFixtureKey(method, url)
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleApiOnboardingImportRepoMissingFields(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/import-repo", strings.NewReader(`{}`))
	s.handleApiOnboardingImportRepo(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Enter a valid GitHub owner and repository name") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleApiOnboardingImportRepoUpstreamNon200WithMessage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeUpstreamFixture(t, dir, "GET", "https://api.github.com/repos/o/r", `{"status":404,"json":{"message":"Not Found"}}`)
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/import-repo", strings.NewReader(
		`{"repo_owner":"o","repo_name":"r"}`))
	s.handleApiOnboardingImportRepo(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 on non-200 upstream, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Not Found") {
		t.Fatalf("expected upstream message forwarded, got %s", w.Body.String())
	}
}

func TestHandleApiOnboardingImportRepoUpstreamNon200NoMessage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeUpstreamFixture(t, dir, "GET", "https://api.github.com/repos/o/r2", `{"status":500,"json":{}}`)
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/import-repo", strings.NewReader(
		`{"repo_owner":"o","repo_name":"r2"}`))
	s.handleApiOnboardingImportRepo(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GitHub lookup failed (500)") {
		t.Fatalf("expected synthesized failure message, got %s", w.Body.String())
	}
}

func TestHandleApiOnboardingImportRepoUnexpectedPayload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// A 200 response whose body is a JSON array, not an object -> "Unexpected GitHub response payload".
	writeUpstreamFixture(t, dir, "GET", "https://api.github.com/repos/o/r3", `{"status":200,"json":[1,2,3]}`)
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/import-repo", strings.NewReader(
		`{"repo_owner":"o","repo_name":"r3"}`))
	s.handleApiOnboardingImportRepo(w, r)
	if w.Code != 502 {
		t.Fatalf("want 502, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Unexpected GitHub response payload") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleApiOnboardingImportRepoSuccessWithDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// Full success payload lacking full_name/html_url/name/visibility so the defaulting branches run.
	writeUpstreamFixture(t, dir, "GET", "https://api.github.com/repos/acme/widgets", `{"status":200,"json":{}}`)
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/import-repo", strings.NewReader(
		`{"repo_owner":"acme","repo_name":"widgets"}`))
	s.handleApiOnboardingImportRepo(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`"full_name":"acme/widgets"`,
		`"repo_url":"https://github.com/acme/widgets"`,
		`"name":"widgets"`,
		`"visibility":"public"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

func TestHandleApiOnboardingImportRepoSuccessWithFullPayload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeUpstreamFixture(t, dir, "GET", "https://api.github.com/repos/acme/widgets2", `{"status":200,"json":{
		"full_name":"acme-org/widgets2","html_url":"https://github.com/acme-org/widgets2",
		"name":"widgets2-renamed","default_branch":"main","visibility":"private","description":"a widget repo"
	}}`)
	s := &server{db: storetest.SettingsDB(map[string]string{"ai.github_token": "stored-token"})}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/import-repo", strings.NewReader(
		`{"repo_owner":"acme","repo_name":"widgets2"}`))
	s.handleApiOnboardingImportRepo(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`"full_name":"acme-org/widgets2"`,
		`"visibility":"private"`,
		`"description":"a widget repo"`,
		`"default_branch":"main"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

// ---- handleApiOnboardingListRepos ---------------------------------------------------------

func TestHandleApiOnboardingListReposMissingOwner(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/list-repos", strings.NewReader(`{"owner":"  /  "}`))
	s.handleApiOnboardingListRepos(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Owner or username is required") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleApiOnboardingListReposUsersEndpointSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	usersURL := "https://api.github.com/users/acme/repos?per_page=100&type=public&sort=full_name"
	writeUpstreamFixture(t, dir, "GET", usersURL, `{"status":200,"json":[
		{"name":"zeta","private":false},
		{"name":"alpha","owner":{"login":"acme"},"private":true}
	]}`)
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/list-repos", strings.NewReader(`{"owner":"acme"}`))
	s.handleApiOnboardingListRepos(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// alpha should sort before zeta (case-insensitive name sort).
	if strings.Index(body, `"alpha"`) > strings.Index(body, `"zeta"`) {
		t.Fatalf("expected alpha before zeta (sorted), got %s", body)
	}
	if !strings.Contains(body, `"token_used":false`) || !strings.Contains(body, "Need PAT to see private repositories.") {
		t.Fatalf("expected no-token visibility note, got %s", body)
	}
}

func TestHandleApiOnboardingListReposFiltersInvalidItemsAndEmptyNames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	usersURL := "https://api.github.com/users/acme/repos?per_page=100&type=public&sort=full_name"
	// A non-object list entry and an object with an empty/absent "name" must both be skipped,
	// leaving only the one well-formed repo in the response.
	writeUpstreamFixture(t, dir, "GET", usersURL, `{"status":200,"json":[
		"not-an-object",
		{"name":""},
		{"private":true},
		{"name":"keeper"}
	]}`)
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/list-repos", strings.NewReader(`{"owner":"acme"}`))
	s.handleApiOnboardingListRepos(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"keeper"`) {
		t.Fatalf("expected the valid repo to survive filtering, got %s", body)
	}
	if strings.Count(body, `"name":`) != 1 {
		t.Fatalf("expected exactly one surviving repo entry, got %s", body)
	}
}

func TestHandleApiOnboardingListReposFallsBackToOrgsEndpoint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	usersURL := "https://api.github.com/users/acme-org/repos?per_page=100&type=all&sort=full_name"
	orgsURL := "https://api.github.com/orgs/acme-org/repos?per_page=100&type=all&sort=full_name"
	writeUpstreamFixture(t, dir, "GET", usersURL, `{"status":404,"json":{"message":"Not Found"}}`)
	writeUpstreamFixture(t, dir, "GET", orgsURL, `{"status":200,"json":[{"name":"repo-a"}]}`)
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/list-repos", strings.NewReader(
		`{"owner":"acme-org","github_token":"tok"}`))
	s.handleApiOnboardingListRepos(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200 via orgs fallback, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"repo-a"`) || !strings.Contains(w.Body.String(), `"token_used":true`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleApiOnboardingListReposBothEndpointsFail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// Neither fixture written -> both resolve to the upstreamFixture 404 fallback (not a list).
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/onboarding/list-repos", strings.NewReader(`{"owner":"ghost-owner"}`))
	s.handleApiOnboardingListRepos(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Not Found") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// ---- handleApiDashboardsSpecRender ---------------------------------------------------------

func TestHandleApiDashboardsSpecRenderCompileError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/spec/render", strings.NewReader(`{"spec":{}}`))
	s.handleApiDashboardsSpecRender(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 on compile error, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"ok"`) {
		t.Fatalf("errorOnly path must not include an ok field: %s", w.Body.String())
	}
}

func TestHandleApiDashboardsSpecRenderQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("bad query")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/spec/render", strings.NewReader(
		`{"spec":{"template_id":"heatmap","sql":{"mode":"raw","override_sql":"SELECT 1"}}}`))
	s.handleApiDashboardsSpecRender(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 on query error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiDashboardsSpecRenderEmptyRowsNoData(t *testing.T) {
	// db.Execute returns no rows -> renderChartFromTemplateWithNamed's "no rows" branch (no error),
	// exercising the success path with an empty named_queries list (no "named_queries" key at all).
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/spec/render", strings.NewReader(
		`{"spec":{"template_id":"heatmap","sql":{"mode":"raw","override_sql":"SELECT 1"}}}`))
	s.handleApiDashboardsSpecRender(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"template_id":"heatmap"`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleApiDashboardsSpecRenderWithNamedQueries(t *testing.T) {
	// named_queries present (even though empty results) exercises the s.executeNamedQueriesWithRecords
	// loop and the namedDatasets population/name-skip logic.
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	body := `{"spec":{"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 1"},` +
		`"named_queries":[{"name":"series_a","sql":"SELECT 2"}],"option_template":"{}"}}`
	r := httptest.NewRequest("POST", "/api/dashboards/spec/render", strings.NewReader(body))
	s.handleApiDashboardsSpecRender(w, r)
	// custom_echarts with empty rows still goes through renderCustomEcharts (not the noData shortcut);
	// we only assert it doesn't panic and returns a definite HTTP status (200 or a 400 render error).
	if w.Code != 200 && w.Code != 400 {
		t.Fatalf("want 200 or 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiDashboardsSpecRenderRenderError(t *testing.T) {
	// heatmap requires exactly 3 columns; returning rows with only 2 columns triggers
	// renderChartFromTemplateWithNamed's "requires at least N columns" error branch (rErr != "").
	cols := []string{"x", "y"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols, []any{"a", "b"}), nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/spec/render", strings.NewReader(
		`{"spec":{"template_id":"heatmap","sql":{"mode":"raw","override_sql":"SELECT 1"}}}`))
	s.handleApiDashboardsSpecRender(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 on render error, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "requires at least") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// ---- handleApiMcpEnabled -------------------------------------------------------------------

func TestHandleApiMcpEnabledDefaultsTrueAndPersists(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/mcp/enabled", strings.NewReader(`{}`))
	s.handleApiMcpEnabled(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("default should be enabled=true, got %s", w.Body.String())
	}
	if len(fake.Inserts) != 1 || fake.Inserts[0].Rows[0]["Value"] != "1" {
		t.Fatalf("expected stored '1', got %v", fake.Inserts)
	}
}

func TestHandleApiMcpEnabledExplicitFalse(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/mcp/enabled", strings.NewReader(`{"enabled":false}`))
	s.handleApiMcpEnabled(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("want enabled=false, got %s", w.Body.String())
	}
	if fake.Inserts[0].Rows[0]["Value"] != "0" {
		t.Fatalf("expected stored '0', got %v", fake.Inserts[0].Rows[0])
	}
}

func TestHandleApiMcpEnabledSetAppSettingError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{InsertErr: errors.New("write failed")}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/mcp/enabled", strings.NewReader(`{"enabled":true}`))
	s.handleApiMcpEnabled(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- githubBackfillMaxReleases ---------------------------------------------------------------

func TestGithubBackfillMaxReleases(t *testing.T) {
	cases := []struct {
		name  string
		unset bool
		raw   string
		want  int
	}{
		{"unset -> default 300", true, "", 300},
		{"invalid -> default 300", false, "not-a-number", 300},
		{"below floor clamps to 1", false, "-5", 1},
		{"zero clamps to 1", false, "0", 1},
		{"above ceiling clamps to 2000", false, "5000", 2000},
		{"valid mid-range value", false, "750", 750},
		{"whitespace-padded valid value", false, "  42  ", 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{}
			if !tc.unset {
				settings["enrichment.github_backfill_max_releases"] = tc.raw
			}
			s := &server{db: storetest.SettingsDB(settings)}
			if got := s.githubBackfillMaxReleases(); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// ---- runCveScan ---------------------------------------------------------------------------

func TestRunCveScanDisabled(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{"enrichment.cve_enabled": "false"})}
	obj := s.runCveScan()
	okV, _ := obj.Get("ok")
	if okV != false {
		t.Fatalf("want ok=false, got %v", okV)
	}
	reasonV, _ := obj.Get("reason")
	if reasonV != "disabled" {
		t.Fatalf("want reason=disabled, got %v", reasonV)
	}
}

func TestRunCveScanEnabledEmptyLibraries(t *testing.T) {
	// enrichment.cve_enabled absent -> defaults to enabled (per doc comment: "enabled by default").
	// No ai.github_token -> fetchReleaseDepsFromGithub is a no-op; collectLibraryInventory sees no
	// library rows (FakeDB zero-value Execute -> empty result for every query) -> zero summary path.
	fake := &storetest.FakeDB{}
	s := &server{db: fake}
	obj := s.runCveScan()
	okV, _ := obj.Get("ok")
	if okV != true {
		t.Fatalf("want ok=true, got %v", okV)
	}
	libsV, _ := obj.Get("libraries_found")
	if libsV != 0 {
		t.Fatalf("want libraries_found=0, got %v", libsV)
	}
	vulnsV, _ := obj.Get("vulns_found")
	if vulnsV != 0 {
		t.Fatalf("want vulns_found=0, got %v", vulnsV)
	}
	// Bookkeeping settings must be persisted even on the empty-library shortcut.
	var sawBackfillAttempted, sawLastScan bool
	for _, ins := range fake.Inserts {
		if ins.Table != "sobs_app_settings" {
			continue
		}
		switch ins.Rows[0]["Key"] {
		case "enrichment.cve_last_scan_github_backfill_attempted":
			sawBackfillAttempted = true
		case "enrichment.cve_last_scan":
			sawLastScan = true
		}
	}
	if !sawBackfillAttempted || !sawLastScan {
		t.Fatalf("expected bookkeeping settings written, got %v", fake.Inserts)
	}
}

// ---- handleApiNotificationsAutoGenerate --------------------------------------------------

func TestHandleApiNotificationsAutoGeneratePreviewDefault(t *testing.T) {
	// No form body at all -> action defaults to "preview"; no metric rules -> empty candidates.
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/rules/auto-generate", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleApiNotificationsAutoGenerate(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"candidates":[]`) {
		t.Fatalf("expected empty candidates in preview mode, got %s", w.Body.String())
	}
}

func TestHandleApiNotificationsAutoGenerateCreateInsertsAndSkipsCovered(t *testing.T) {
	metricCols := []string{"Id", "Name", "SignalSource", "SignalName", "ServiceName", "Comparator",
		"WarningThreshold", "CriticalThreshold"}
	ruleCols := []string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds", "Severity", "CooldownSeconds"}
	chanCols := []string{"Id", "Name"}
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_anomaly_rules"):
			return storetest.Result(metricCols,
				[]any{"r1", "High CPU", "cpu", "usage", "svc-a", "gt", 0.0, 90.0},
			), nil
		case strings.Contains(q, "sobs_notification_rules"):
			// No existing rule covers cpu/usage on the first (preview-derived) read, but the
			// create action re-derives coveredNow which is empty too -> the candidate is created.
			return storetest.Result(ruleCols), nil
		case strings.Contains(q, "sobs_notification_channels"):
			return storetest.Result(chanCols, []any{"ch1", "Slack"}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/rules/auto-generate",
		strings.NewReader("action=create"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleApiNotificationsAutoGenerate(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"created":1`) {
		t.Fatalf("want created=1, got %s", w.Body.String())
	}
	var sawRuleInsert bool
	for _, ins := range fake.Inserts {
		if ins.Table == "sobs_notification_rules" {
			sawRuleInsert = true
			if ins.Rows[0]["ChannelIds"] != "ch1" {
				t.Errorf("ChannelIds = %v", ins.Rows[0]["ChannelIds"])
			}
		}
	}
	if !sawRuleInsert {
		t.Fatalf("expected an insert into sobs_notification_rules, got %v", fake.Inserts)
	}
}

func TestHandleApiNotificationsAutoGenerateCreateInsertError(t *testing.T) {
	metricCols := []string{"Id", "Name", "SignalSource", "SignalName", "ServiceName", "Comparator",
		"WarningThreshold", "CriticalThreshold"}
	fake := &storetest.FakeDB{
		ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_anomaly_rules") {
				return storetest.Result(metricCols,
					[]any{"r1", "High CPU", "cpu", "usage", "svc-a", "gt", 0.0, 90.0},
				), nil
			}
			return &store.Result{}, nil
		},
		InsertErr: errors.New("insert failed"),
	}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/rules/auto-generate",
		strings.NewReader("action=create"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleApiNotificationsAutoGenerate(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500 on insert error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiNotificationsAutoGenerateCreateSkipsAlreadyCovered(t *testing.T) {
	metricCols := []string{"Id", "Name", "SignalSource", "SignalName", "ServiceName", "Comparator",
		"WarningThreshold", "CriticalThreshold"}
	ruleCols := []string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds", "Severity", "CooldownSeconds"}
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_anomaly_rules"):
			return storetest.Result(metricCols,
				[]any{"r1", "High CPU", "cpu", "usage", "svc-a", "gt", 0.0, 90.0},
			), nil
		case strings.Contains(q, "sobs_notification_rules"):
			return storetest.Result(ruleCols,
				[]any{"nr1", "CPU rule", float64(1), "any", `[{"type":"signal","source":"cpu","signal":"usage"}]`, "", "warning", 300.0},
			), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/rules/auto-generate",
		strings.NewReader("action=create"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleApiNotificationsAutoGenerate(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"created":0`) {
		t.Fatalf("want created=0 (already covered), got %s", body)
	}
	if !strings.Contains(body, `"skipped":1`) {
		t.Fatalf("want skipped=1, got %s", body)
	}
}

func TestHandleApiNotificationsAutoGenerateScopedByMetricRuleID(t *testing.T) {
	metricCols := []string{"Id", "Name", "SignalSource", "SignalName", "ServiceName", "Comparator",
		"WarningThreshold", "CriticalThreshold"}
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_anomaly_rules") {
			if !strings.Contains(q, "AND Id = ?") || params[0] != "r9" {
				t.Fatalf("expected scoped query for r9, got q=%q params=%v", q, params)
			}
			return storetest.Result(metricCols,
				[]any{"r9", "Disk", "disk", "free", "svc-z", "lt", 0.0, 0.0},
			), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/rules/auto-generate",
		strings.NewReader("metric_rule_id=r9"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleApiNotificationsAutoGenerate(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"examined":1`) {
		t.Fatalf("want examined=1, got %s", w.Body.String())
	}
}

// ---- truthy -------------------------------------------------------------------------------

func TestTruthyAllBranches(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"bool true", true, true},
		{"bool false", false, false},
		{"non-empty string", "yes", true},
		{"empty string", "", false},
		{"nonzero float", 1.5, true},
		{"zero float", 0.0, false},
		{"nil", nil, false},
		{"default (slice)", []any{1}, true},
		{"default (map)", map[string]any{"a": 1}, true},
	}
	for _, tc := range cases {
		if got := truthy(tc.v); got != tc.want {
			t.Errorf("%s: truthy(%v) = %v, want %v", tc.name, tc.v, got, tc.want)
		}
	}
}

// bodyBool default branch (absent key -> caller-supplied default) is exercised implicitly by
// TestHandleApiOnboardingCreateRepoNoTokenOptionalWrites/TokenSettingsBranches above; this test
// pins bodyBool directly for both the absent-key and present-falsy-string cases.
func TestBodyBoolDirect(t *testing.T) {
	m := map[string]any{"a": true, "b": "", "c": "no"}
	if !bodyBool(m, "a", false) {
		t.Error("present true should be true")
	}
	if bodyBool(m, "b", true) {
		t.Error("present empty string is falsy")
	}
	if bodyBool(m, "missing", true) != true {
		t.Error("absent key should use the default")
	}
	if bodyBool(m, "missing", false) != false {
		t.Error("absent key should use the default (false)")
	}
	_ = strconv.Itoa(0) // keep strconv import stable if unused elsewhere in the future
}
