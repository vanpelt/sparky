package host

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestObserveHiveMindSessionsKeepsAnEphemeralCopy(t *testing.T) {
	m := &Manager{boxes: map[string]*Sandbox{
		"dev": {ID: "box-id", Name: "dev"},
	}}
	snapshot := HiveMindSessionSnapshot{
		ObservedAt: time.Now().UTC(),
		TotalCount: 1,
		Sessions: []HiveMindSession{{
			ID: "session-1", Title: "Original", URL: "https://example/sessions/session-1",
		}},
	}

	m.ObserveHiveMindSessions("box-id", snapshot)
	snapshot.Sessions[0].Title = "mutated caller"

	got, ok := m.HiveMindSessions("box-id")
	if !ok || got.Sessions[0].Title != "Original" {
		t.Fatalf("snapshot = %+v, found %v", got, ok)
	}
	got.Sessions[0].Title = "mutated reader"
	again, _ := m.HiveMindSessions("box-id")
	if again.Sessions[0].Title != "Original" {
		t.Fatalf("stored snapshot changed through returned slice: %+v", again)
	}

	box, _ := m.Get("dev")
	data, err := json.Marshal(box)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || !json.Valid(data) {
		t.Fatalf("invalid sandbox JSON: %s", data)
	}
	if bytes.Contains(data, []byte("session-1")) {
		t.Fatalf("ephemeral HiveMind data leaked into persisted JSON: %s", data)
	}
}
