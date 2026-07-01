package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// seedAppRegistry only runs when SOBS_APP_REGISTRY_SEED_JSON is set, which the byte-parity corpus
// never does — so its parse/dedup branches are corpus-unreachable. Drive them directly.
// Oracle: app.py _seed_app_release_registry_from_env.

func TestSeedAppRegistry_UnsetEnv_NoOp(t *testing.T) {
	fake := &storetest.FakeDB{}
	(&server{db: fake}).seedAppRegistry()
	if len(fake.Inserts) != 0 {
		t.Fatalf("unset env: want no inserts, got %v", fake.Inserts)
	}
}

func TestSeedAppRegistry_InvalidJSON(t *testing.T) {
	t.Setenv("SOBS_APP_REGISTRY_SEED_JSON", "{not json")
	fake := &storetest.FakeDB{}
	(&server{db: fake}).seedAppRegistry()
	if len(fake.Inserts) != 0 {
		t.Fatalf("invalid json: want no inserts, got %v", fake.Inserts)
	}
}

func TestSeedAppRegistry_UnexpectedShape(t *testing.T) {
	t.Setenv("SOBS_APP_REGISTRY_SEED_JSON", `"just a string"`)
	fake := &storetest.FakeDB{}
	(&server{db: fake}).seedAppRegistry()
	if len(fake.Inserts) != 0 {
		t.Fatalf("unexpected shape: want no inserts, got %v", fake.Inserts)
	}
}

func TestSeedAppRegistry_FullGraph(t *testing.T) {
	payload := `{
		"apps": [
			{"id": "app-explicit", "name": "Explicit App"},
			{"name": "New App", "slug": "new-app"},
			"not-a-map",
			{"name": ""},
			{
				"name": "Existing App", "slug": "existing-app",
				"releases": [
					{"version": "2.0.0", "commitSha": "c1", "environment": "prod"},
					{"version": "3.0.0", "artifacts": [
						{"artifactType": "", "name": "x"},
						{"artifactType": "docker", "name": ""},
						{"artifactType": "docker", "name": "good", "size": 42}
					]},
					{"version": ""},
					"not-a-map"
				]
			}
		]
	}`
	t.Setenv("SOBS_APP_REGISTRY_SEED_JSON", payload)

	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_apps"):
			if len(params) == 1 && params[0] == "existing-app" {
				return storetest.Result([]string{"Id"}, []any{"app-existing-id"}), nil
			}
			return &store.Result{}, nil
		case strings.Contains(q, "sobs_app_releases"):
			if len(params) == 4 && params[0] == "app-existing-id" && params[1] == "2.0.0" {
				return storetest.Result([]string{"Id"}, []any{"rel-existing-id"}), nil
			}
			return &store.Result{}, nil
		}
		return &store.Result{}, nil
	}}
	(&server{db: fake}).seedAppRegistry()

	var apps, releases, artifacts []map[string]any
	for _, ins := range fake.Inserts {
		switch ins.Table {
		case "sobs_apps":
			apps = ins.Rows
		case "sobs_app_releases":
			releases = ins.Rows
		case "sobs_release_artifacts":
			artifacts = ins.Rows
		}
	}

	if len(apps) != 3 {
		t.Fatalf("want 3 app rows (explicit id, new, existing), got %d: %v", len(apps), apps)
	}
	byName := map[string]map[string]any{}
	for _, a := range apps {
		byName[a["Name"].(string)] = a
	}
	if byName["Explicit App"]["Id"] != "app-explicit" || byName["Explicit App"]["Slug"] != "explicit-app" {
		t.Fatalf("explicit-id app wrong: %v", byName["Explicit App"])
	}
	if byName["Existing App"]["Id"] != "app-existing-id" {
		t.Fatalf("existing app should reuse looked-up id, got %v", byName["Existing App"])
	}
	if newID, _ := byName["New App"]["Id"].(string); newID == "" {
		t.Fatalf("new app should get a generated id, got %v", byName["New App"])
	}

	if len(releases) != 2 {
		t.Fatalf("want 2 release rows (empty version + non-map skipped), got %d: %v", len(releases), releases)
	}
	byVersion := map[string]map[string]any{}
	for _, r := range releases {
		byVersion[r["ReleaseVersion"].(string)] = r
	}
	if byVersion["2.0.0"]["Id"] != "rel-existing-id" {
		t.Fatalf("release 2.0.0 should reuse looked-up id, got %v", byVersion["2.0.0"])
	}
	if relID, _ := byVersion["3.0.0"]["Id"].(string); relID == "" {
		t.Fatalf("release 3.0.0 should get a generated id, got %v", byVersion["3.0.0"])
	}

	if len(artifacts) != 1 {
		t.Fatalf("want 1 artifact row (empty type/name skipped), got %d: %v", len(artifacts), artifacts)
	}
	if artifacts[0]["Name"] != "good" || artifacts[0]["Size"] != 42 {
		t.Fatalf("artifact row wrong: %v", artifacts[0])
	}
	if artID, _ := artifacts[0]["Id"].(string); artID == "" {
		t.Fatalf("artifact should get a generated id, got %v", artifacts[0])
	}
}
