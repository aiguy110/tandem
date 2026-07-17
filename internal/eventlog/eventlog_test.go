package eventlog

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/store"
	_ "modernc.org/sqlite"
)

func openLog(t *testing.T, capacity int) (*Log, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tandem.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	log, err := New("api-1", s, capacity)
	if err != nil {
		t.Fatal(err)
	}
	return log, s, path
}

func message(t *testing.T, text string) Event {
	t.Helper()
	event, err := ParseNormalized([]byte(`{"kind":"message_chunk","text":` + string(mustJSON(t, text)) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEncodingAndAppend(t *testing.T) {
	log, _, _ := openLog(t, 4)
	log.now = func() time.Time { return time.UnixMilli(1700000010000) }
	ordinary := message(t, "working")
	first, err := log.Append(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	second, err := log.Append(RawPTY([]byte{0, 10, 255}))
	if err != nil {
		t.Fatal(err)
	}
	third, err := log.Append(ShellPTY([]byte("shell\x00")))
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 || third.Seq != 3 || first.TS != 1700000010000 || log.Head() != 3 {
		t.Fatalf("first=%#v second=%#v third=%#v head=%d", first, second, third, log.Head())
	}
	history, err := log.FullHistory()
	if err != nil {
		t.Fatal(err)
	}
	normalized, _ := history[1].Event.NormalizedJSON()
	if string(normalized) != `{"kind":"raw_pty","dataB64":"AAr/"}` || !reflect.DeepEqual(history[1].Event.Data, []byte{0, 10, 255}) {
		t.Fatalf("raw event JSON=%s data=%v", normalized, history[1].Event.Data)
	}
	if _, err := ParseNormalized([]byte(`{"kind":"raw_pty","dataB64":"%%%"}`)); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	shellNormalized, _ := history[2].Event.NormalizedJSON()
	if string(shellNormalized) != `{"kind":"shell_pty","dataB64":"c2hlbGwA"}` || !reflect.DeepEqual(history[2].Event.Data, []byte("shell\x00")) {
		t.Fatalf("shell event JSON=%s data=%v", shellNormalized, history[2].Event.Data)
	}
	if _, err := ParseNormalized([]byte(`{"kind":"shell_pty","dataB64":"%%%"}`)); err == nil {
		t.Fatal("invalid shell base64 accepted")
	}
}

func TestRingEvictionColdReplayAndRestart(t *testing.T) {
	log, s, _ := openLog(t, 2)
	for _, text := range []string{"one", "two", "three", "four"} {
		if _, err := log.Append(message(t, text)); err != nil {
			t.Fatal(err)
		}
	}
	if got := log.EarliestInRing(); got != 3 {
		t.Fatalf("earliest=%d want 3", got)
	}
	hot, err := log.ReplaySince(2)
	if err != nil || hot.Source != ReplayHot || seqs(hot.Events) != "3,4" {
		t.Fatalf("hot=%#v err=%v", hot, err)
	}
	cold, err := log.ReplaySince(1)
	if err != nil || cold.Source != ReplayCold || seqs(cold.Events) != "2,3,4" {
		t.Fatalf("cold=%#v err=%v", cold, err)
	}
	restarted, err := New("api-1", s, 2)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Head() != 4 || restarted.EarliestInRing() != 5 {
		t.Fatalf("restart head=%d earliest=%d", restarted.Head(), restarted.EarliestInRing())
	}
	fromDisk, err := restarted.ReplaySince(2)
	if err != nil || fromDisk.Source != ReplayCold || seqs(fromDisk.Events) != "3,4" {
		t.Fatalf("restart replay=%#v err=%v", fromDisk, err)
	}
	fresh, err := restarted.ReplaySince(0)
	if err != nil || fresh.Source != ReplaySnapshot || seqs(fresh.Events) != "1,2,3,4" {
		t.Fatalf("snapshot=%#v err=%v", fresh, err)
	}
	future, err := restarted.ReplaySince(10)
	if err != nil || future.Source != ReplaySnapshot || seqs(future.Events) != "1,2,3,4" {
		t.Fatalf("future snapshot=%#v err=%v", future, err)
	}
}

func TestPrunedHistorySignalsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE events (agentId TEXT NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL, payload TEXT NOT NULL, ts INTEGER NOT NULL, PRIMARY KEY(agentId,seq));
INSERT INTO events VALUES ('api-1', 3, 'message_chunk', '{"kind":"message_chunk","text":"three"}', 3), ('api-1', 4, 'message_chunk', '{"kind":"message_chunk","text":"four"}', 4)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	log, err := New("api-1", s, 2)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := log.ReplaySince(1)
	if err != nil || replay.Source != ReplaySnapshot || seqs(replay.Events) != "3,4" {
		t.Fatalf("gap replay=%#v err=%v", replay, err)
	}
	covered, err := log.ReplaySince(2)
	if err != nil || covered.Source != ReplayCold || seqs(covered.Events) != "3,4" {
		t.Fatalf("covered replay=%#v err=%v", covered, err)
	}
}

func TestIndependentAndConcurrentSequences(t *testing.T) {
	logA, s, _ := openLog(t, 256)
	logB, err := New("api-2", s, 256)
	if err != nil {
		t.Fatal(err)
	}
	const count = 100
	var wg sync.WaitGroup
	returned := make(chan int64, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			logged, err := logA.Append(message(t, string(rune('a'+i%26))))
			if err != nil {
				t.Errorf("append %d: %v", i, err)
				return
			}
			returned <- logged.Seq
		}(i)
	}
	wg.Wait()
	close(returned)
	got := make([]int, 0, count)
	for seq := range returned {
		got = append(got, int(seq))
	}
	sort.Ints(got)
	for i, seq := range got {
		if seq != i+1 {
			t.Fatalf("concurrent sequences[%d]=%d", i, seq)
		}
	}
	if _, err := logB.Append(message(t, "independent")); err != nil {
		t.Fatal(err)
	}
	if logA.Head() != count || logB.Head() != 1 {
		t.Fatalf("heads A=%d B=%d", logA.Head(), logB.Head())
	}
}

func TestMultipleLogsCannotReuseSequence(t *testing.T) {
	_, s, _ := openLog(t, 16)
	a, _ := New("shared", s, 16)
	b, _ := New("shared", s, 16)
	const count = 40
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := a.Append(message(t, "a")); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := b.Append(message(t, "b")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	rows, err := s.RangeEvents("shared", 0)
	if err != nil || len(rows) != count*2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	for i, row := range rows {
		if row.Seq != int64(i+1) {
			t.Fatalf("row %d seq=%d", i, row.Seq)
		}
	}
}

func jsonEqual(a, b []byte) bool {
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}

func seqs(events []LoggedEvent) string {
	if len(events) == 0 {
		return ""
	}
	b := make([]byte, 0, len(events)*2)
	for i, event := range events {
		if i != 0 {
			b = append(b, ',')
		}
		b = append(b, byte('0'+event.Seq))
	}
	return string(b)
}
