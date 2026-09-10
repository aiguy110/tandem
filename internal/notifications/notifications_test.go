package notifications

import "testing"

func TestCenterBroadcastsCompleteOrderedSnapshot(t *testing.T) {
	center := New()
	var got []Notification
	off := center.Subscribe(func(items []Notification) { got = items })
	center.Upsert(Notification{ID: "one", Title: "First"})
	center.Upsert(Notification{ID: "two", Title: "Second"})
	center.Upsert(Notification{ID: "one", Title: "Updated"})
	if len(got) != 2 || got[0].Title != "Updated" || got[1].ID != "two" {
		t.Fatalf("snapshot = %#v", got)
	}
	center.Remove("one")
	if len(got) != 1 || got[0].ID != "two" {
		t.Fatalf("after remove = %#v", got)
	}
	off()
	center.Remove("two")
	if len(got) != 1 {
		t.Fatal("unsubscribed callback was invoked")
	}
}
