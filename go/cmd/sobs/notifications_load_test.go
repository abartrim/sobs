package main

import (
	"errors"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// loadNotificationChannels reads sobs_notification_channels and shapes each row (JSON config parse,
// non-sensitive config passthrough, enabled coercion). The settings page reaches it only with the
// notif profile's seeded rows; its empty-config and query-error branches are corpus-unreachable.
// Oracle: app.py _load_notification_channels.
func TestLoadNotificationChannels(t *testing.T) {
	cols := []string{"Id", "Name", "ChannelType", "ConfigJson", "Enabled"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols,
			[]any{"c1", "Slack", "slack", `{"url":"http://hook"}`, float64(1)}, // valid config, enabled
			[]any{"c2", "Web", "webhook", "", float64(0)},                      // empty config → NewObject, disabled
		), nil
	}}}
	got := s.loadNotificationChannels()
	if len(got) != 2 {
		t.Fatalf("want 2 channels, got %d", len(got))
	}
	r0 := got[0].(map[string]any)
	if r0["id"] != "c1" || r0["channel_type"] != "slack" || r0["enabled"] != true {
		t.Fatalf("row0 shape wrong: %v", r0)
	}
	r1 := got[1].(map[string]any)
	if r1["id"] != "c2" || r1["enabled"] != false {
		t.Fatalf("row1 shape wrong: %v", r1)
	}

	// Query error → empty (non-nil) slice.
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadNotificationChannels(); len(got) != 0 {
		t.Fatalf("query error: want empty, got %v", got)
	}
}

// loadNotificationChannelsByID builds the id->channel map used by the rule-check path; corpus-
// unreachable on its error branch. Oracle: app.py _load_notification_channels_by_id.
func TestLoadNotificationChannelsByID(t *testing.T) {
	cols := []string{"Id", "Name", "ChannelType", "ConfigJson", "Enabled"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols,
			[]any{"c1", "Slack", "slack", `{"url":"http://hook"}`, float64(1)},
			[]any{"c2", "Web", "webhook", "{}", float64(0)},
		), nil
	}}}
	got := s.loadNotificationChannelsByID()
	if len(got) != 2 {
		t.Fatalf("want 2 channels, got %d", len(got))
	}
	if c := got["c1"]; c.name != "Slack" || c.channelType != "slack" || !c.enabled {
		t.Fatalf("c1 wrong: %+v", c)
	}
	if c := got["c2"]; c.enabled {
		t.Fatalf("c2 should be disabled: %+v", c)
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadNotificationChannelsByID(); len(got) != 0 {
		t.Fatalf("query error: want empty map, got %v", got)
	}
}
