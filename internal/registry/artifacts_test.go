package registry

import (
	"encoding/json"
	"testing"

	"github.com/aiguy110/tandem/internal/store"
)

func TestArtifactsListsNewestFirstAcrossSessionsAndDedupes(t *testing.T) {
	r := testRegistry(t)
	for _, id := range []string{"s1", "s2"} {
		if err := r.store.UpsertSession(store.Session{ID: id, Name: "name-" + id, Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent := func(session, kind, payload string, ts int64) {
		t.Helper()
		if _, err := r.store.AppendEvent(session, kind, payload, ts); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent("s1", "artifact", `{"kind":"artifact","artifactId":"a","title":"first"}`, 10)
	appendEvent("s1", "agent_message", `{"kind":"agent_message"}`, 11)
	appendEvent("s2", "artifact", `{"kind":"artifact","artifactId":"b","title":"second"}`, 20)
	appendEvent("s1", "artifact", `{"kind":"artifact","artifactId":"a","title":"first again"}`, 30)
	appendEvent("s2", "artifact", `{"kind":"artifact","artifactId":"a","title":"same bytes, other session"}`, 40)

	list, err := r.Artifacts(0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range list {
		var a struct{ Title string }
		if err := json.Unmarshal(item.Artifact, &a); err != nil {
			t.Fatal(err)
		}
		got = append(got, item.SessionName+":"+a.Title)
	}
	want := []string{"name-s2:same bytes, other session", "name-s1:first again", "name-s2:second"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if limited, err := r.Artifacts(1); err != nil || len(limited) != 1 {
		t.Fatalf("limit 1: %v %v", limited, err)
	}
}
