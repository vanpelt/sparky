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
	endedAt := time.Now().UTC().Add(-time.Minute)
	wantEndedAt := endedAt
	snapshot := HiveMindSessionSnapshot{
		ObservedAt: time.Now().UTC(),
		TotalCount: 1,
		Sessions: []HiveMindSession{{
			ID: "session-1", Title: "Original", URL: "https://example/sessions/session-1",
			EndedAt: &endedAt,
		}},
	}

	m.ObserveHiveMindSessions("box-id", snapshot)
	snapshot.Sessions[0].Title = "mutated caller"
	*snapshot.Sessions[0].EndedAt = time.Time{}

	got, ok := m.HiveMindSessions("box-id")
	if !ok || got.Sessions[0].Title != "Original" {
		t.Fatalf("snapshot = %+v, found %v", got, ok)
	}
	got.Sessions[0].Title = "mutated reader"
	*got.Sessions[0].EndedAt = time.Time{}
	again, _ := m.HiveMindSessions("box-id")
	if again.Sessions[0].Title != "Original" || !again.Sessions[0].EndedAt.Equal(wantEndedAt) {
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
