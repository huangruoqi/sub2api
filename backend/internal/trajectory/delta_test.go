package trajectory

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Simulated Claude-Code traffic goes through the deduper exactly as run() does
// (batches, line numbers, keys set at flush). Every record must expand back to
// its original body, and the archive must be much smaller than full bodies.
func TestDeltaRoundTrip(t *testing.T) {
	type stored struct {
		key  string
		line int
		want string
	}
	objects := map[string][]json.RawMessage{}
	var all []stored
	dd := newDeduper()
	cur, batchN, rawBytes, storedBytes := new(string), 0, 0, 0
	flush := func() {
		*cur = fmt.Sprintf("trajectories/data/b%d.jsonl.zst", batchN)
		batchN++
		cur = new(string)
	}
	emit := func(session string, body map[string]any) {
		b, _ := json.Marshal(body)
		rec := &Record{Meta: Meta{SessionID: session, APIKeyID: 7, Path: "/v1/messages"}, RequestBody: Body{Data: b}}
		line := len(objects[fmt.Sprint(batchN)])
		dd.process(rec, line, cur, time.Now())
		enc, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		objects[fmt.Sprint(batchN)] = append(objects[fmt.Sprint(batchN)], enc)
		all = append(all, stored{fmt.Sprint(batchN), line, string(b)})
		rawBytes += len(b)
		storedBytes += len(enc)
	}

	tools := strings.Repeat(`tool schema `, 2000)
	msg := func(role string, i int) map[string]any {
		return map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("%s turn %d %s", role, i, strings.Repeat("x", 500))}}}
	}
	var main []any
	for turn := 0; turn < 80; turn++ {
		main = append(main, msg("user", turn))
		msgs := make([]any, len(main))
		copy(msgs, main)
		// cache_control rides on the last message, like Claude Code.
		last := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": fmt.Sprint("user turn ", turn, " ", strings.Repeat("x", 500)), "cache_control": map[string]any{"type": "ephemeral"}}}}
		msgs[len(msgs)-1] = last
		body := map[string]any{"model": "claude", "system": "sys", "tools": tools, "messages": msgs, "max_tokens": 1000, "temperature": 1}
		if turn >= 5 {
			body["max_tokens"] = 2000
		}
		if turn >= 7 {
			delete(body, "temperature")
		}
		emit("s1", body)
		if turn == 9 { // regenerate: drop the last turn and resend
			main = main[:len(main)-1]
			emit("s1", body)
		}
		if turn%3 == 0 { // subagent in the same session, different first message
			emit("s1", map[string]any{"model": "haiku", "messages": []any{msg("user", 1000+turn), msg("assistant", turn), msg("user", turn)}})
		}
		main = append(main, msg("assistant", turn))
		if turn%10 == 9 {
			flush()
		}
	}
	flush()

	// Rename batches to the keys they were given at flush.
	byKey := map[string][]json.RawMessage{}
	for i := 0; i < batchN; i++ {
		byKey[fmt.Sprintf("trajectories/data/b%d.jsonl.zst", i)] = objects[fmt.Sprint(i)]
	}
	fetch := func(key string, lines []int) (map[int]json.RawMessage, error) {
		out := map[int]json.RawMessage{}
		for _, l := range lines {
			if obj, ok := byKey[key]; ok && l < len(obj) {
				out[l] = obj[l]
			}
		}
		return out, nil
	}

	deltas := 0
	for i, s := range all {
		key := "trajectories/data/b" + s.key + ".jsonl.zst"
		raw := byKey[key][s.line]
		if strings.Contains(string(raw), `"request_delta"`) {
			deltas++
		}
		got, err := expand(key, raw, fetch)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		var rec struct {
			RequestBody  any `json:"request_body"`
			RequestDelta any `json:"request_delta"`
		}
		if err := json.Unmarshal(got, &rec); err != nil || rec.RequestDelta != nil {
			t.Fatalf("record %d: %v delta=%v", i, err, rec.RequestDelta)
		}
		var want any
		_ = json.Unmarshal([]byte(s.want), &want)
		if !reflect.DeepEqual(rec.RequestBody, want) {
			t.Fatalf("record %d: rebuilt body differs", i)
		}
	}
	t.Logf("%d records, %d deltas, raw %d KB -> stored %d KB (x%.1f)", len(all), deltas, rawBytes>>10, storedBytes>>10, float64(rawBytes)/float64(storedBytes))
	if deltas < len(all)/2 || storedBytes*5 > rawBytes {
		t.Fatalf("dedup too weak: %d/%d deltas, %d -> %d bytes", deltas, len(all), rawBytes, storedBytes)
	}
}

func TestDeltaSkipsNonConversations(t *testing.T) {
	dd := newDeduper()
	for _, body := range []string{`not json`, `[1,2]`, `{"input":"just a string"}`, `{"messages":[]}`} {
		for i := 0; i < 2; i++ {
			rec := &Record{RequestBody: Body{Data: []byte(body)}}
			dd.process(rec, i, new(string), time.Now())
			if rec.RequestDelta != nil || string(rec.RequestBody.Data) != body {
				t.Fatalf("%s: should be stored as is", body)
			}
		}
	}
	rec := &Record{RequestBody: Body{Data: []byte(`{"messages":[1]}`), Truncated: true}}
	dd.process(rec, 0, new(string), time.Now())
	if rec.RequestDelta != nil {
		t.Fatal("truncated body must not become a delta")
	}
}

// Deleting an object that a chain goes through must not hide the record.
func TestDeltaBrokenChainKeepsRecord(t *testing.T) {
	dd := newDeduper()
	cur := new(string)
	var lines []json.RawMessage
	for i, body := range []string{`{"messages":[1]}`, `{"messages":[1,2]}`} {
		rec := &Record{Meta: Meta{RequestID: fmt.Sprint(i)}, RequestBody: Body{Data: []byte(body)}}
		dd.process(rec, i, cur, time.Now())
		b, _ := json.Marshal(rec)
		lines = append(lines, b)
	}
	gone := func(string, []int) (map[int]json.RawMessage, error) { return nil, fmt.Errorf("NoSuchKey") }
	got, err := expandOrKeep("k", lines[1], gone)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		RequestID string `json:"request_id"`
		Delta     *Delta `json:"request_delta"`
		Error     string `json:"request_rebuild_error"`
	}
	if err := json.Unmarshal(got, &rec); err != nil || rec.RequestID != "1" || rec.Delta == nil || len(rec.Delta.Append) != 1 || !strings.Contains(rec.Error, "NoSuchKey") {
		t.Fatalf("got %s (%v)", got, err)
	}
}
