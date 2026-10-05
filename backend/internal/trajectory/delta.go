package trajectory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/maphash"
	"slices"
	"time"
)

// Agent clients (Claude Code, Codex, ...) resend the whole conversation on every
// turn, so a session's archive grows with the square of its length. A request that
// extends one we already stored is written as a Delta instead of a full
// request_body: top-level keys that changed, how many array elements to keep from
// the parent, and the new ones. Every maxChain requests a full copy is written
// (keyframe), so rebuilding one record reads at most maxChain lines.
//
// The rebuilt body is the same JSON value as the original. Bytes outside the
// top-level values (whitespace, separators) may differ.

// ponytail: fixed knobs. A higher maxChain gives a smaller archive but a slower detail view.
const (
	maxChain     = 32
	headsPerConv = 4 // forks / retries / subagents sharing a first message
	headTTL      = 30 * time.Minute
	maxConvs     = 200_000 // ponytail: reset-on-full; nothing breaks, the next requests are keyframes
)

// arrayFields are the conversation arrays of the Anthropic, OpenAI chat/responses
// and Gemini request shapes, in lookup order.
var arrayFields = []string{"messages", "input", "contents"}

// Delta stands in for request_body when the request extends an earlier one.
type Delta struct {
	Chain  []Ref                      `json:"chain"` // keyframe first, parent last
	Keys   []string                   `json:"keys"`  // top-level key order of this body
	Set    map[string]json.RawMessage `json:"set,omitempty"`
	Field  string                     `json:"field"`
	Keep   int                        `json:"keep"` // first Keep elements of the parent's Field
	Append []json.RawMessage          `json:"append"`
}

// Ref points at a line of a data object. Key "" means the object holding the
// record that contains the Ref.
type Ref struct {
	Key  string `json:"key,omitempty"`
	Line int    `json:"line"`
}

type convKey struct {
	apiKey        int64
	session, path string
	first         uint64
}

// head is what we remember about one stored request, enough to diff the next one.
type head struct {
	vals  map[string]uint64 // top-level value hashes, Field excluded
	field string
	msgs  []uint64
	chain []headRef // keyframe .. this record
	used  time.Time
}

// headRef.key is shared by every record of a batch and set once that batch has
// been spooled, so it is "" exactly while the batch is still being written.
type headRef struct {
	key  *string
	line int
}

// deduper is owned by the recorder goroutine; it is not safe for concurrent use.
type deduper struct {
	seed  maphash.Seed
	heads map[convKey][]*head
}

func newDeduper() *deduper {
	return &deduper{seed: maphash.MakeSeed(), heads: map[convKey][]*head{}}
}

// reset forgets every head. Call it whenever a written line might not exist
// (lost batch, failed encode), so nothing can reference it.
func (d *deduper) reset() { d.heads = map[convKey][]*head{} }

func (d *deduper) evict(now time.Time) {
	if len(d.heads) > maxConvs {
		d.reset()
		return
	}
	for k, hs := range d.heads {
		hs = slices.DeleteFunc(hs, func(h *head) bool { return now.Sub(h.used) > headTTL })
		if len(hs) == 0 {
			delete(d.heads, k)
		} else {
			d.heads[k] = hs
		}
	}
}

// process turns rec's request body into a Delta when it extends a remembered
// request, and remembers rec as line `line` of the batch whose key is cur.
func (d *deduper) process(rec *Record, line int, cur *string, now time.Time) {
	if rec.RequestBody.Truncated {
		return
	}
	keys, vals, ok := splitObject(rec.RequestBody.Data)
	if !ok {
		return
	}
	fi := -1
	for _, f := range arrayFields {
		if i := slices.Index(keys, f); i >= 0 && bytes.HasPrefix(vals[i], []byte("[")) {
			fi = i
			break
		}
	}
	if fi < 0 {
		return
	}
	var elems []json.RawMessage
	if json.Unmarshal(vals[fi], &elems) != nil || len(elems) == 0 {
		return
	}

	h := &head{vals: make(map[string]uint64, len(keys)), field: keys[fi], msgs: make([]uint64, len(elems)), used: now}
	for i, k := range keys {
		if i != fi {
			h.vals[k] = maphash.Bytes(d.seed, vals[i])
		}
	}
	for i, e := range elems {
		h.msgs[i] = maphash.Bytes(d.seed, e)
	}
	ck := convKey{rec.APIKeyID, rec.SessionID, rec.Path, h.msgs[0]}

	hs := d.heads[ck]
	best, keep := -1, 0
	for i, p := range hs {
		if p.field != h.field || len(p.chain) >= maxChain {
			continue
		}
		n := 0
		for n < len(p.msgs) && n < len(h.msgs) && p.msgs[n] == h.msgs[n] {
			n++
		}
		if n > keep {
			best, keep = i, n
		}
	}

	if best < 0 {
		h.chain = []headRef{{cur, line}}
		if len(hs) >= headsPerConv {
			hs = slices.Delete(hs, 0, 1) // oldest
		}
		d.heads[ck] = append(hs, h)
		return
	}

	p := hs[best]
	delta := &Delta{Keys: keys, Field: h.field, Keep: keep, Append: elems[keep:], Chain: make([]Ref, len(p.chain))}
	for i, ref := range p.chain {
		if ref.key != cur {
			delta.Chain[i].Key = *ref.key
		}
		delta.Chain[i].Line = ref.line
	}
	for i, k := range keys {
		if pv, ok := p.vals[k]; i != fi && (!ok || pv != h.vals[k]) {
			if delta.Set == nil {
				delta.Set = map[string]json.RawMessage{}
			}
			delta.Set[k] = vals[i]
		}
	}
	rec.RequestDelta, rec.RequestBody = delta, Body{}
	h.chain = append(slices.Clip(p.chain), headRef{cur, line})
	hs[best] = h
}

// splitObject returns the top-level keys and raw values of a JSON object, in order.
func splitObject(b []byte) ([]string, []json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, false
	}
	var keys []string
	var vals []json.RawMessage
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, false
		}
		keys, vals = append(keys, k), append(vals, v)
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, nil, false
	}
	return keys, vals, true
}

// bodyState is a request body being rebuilt along a chain.
type bodyState struct {
	keys []string
	vals map[string]json.RawMessage
	arr  []json.RawMessage // value of the Field key
}

func stateOf(body json.RawMessage, field string) (*bodyState, error) {
	keys, vals, ok := splitObject(body)
	if !ok {
		return nil, fmt.Errorf("keyframe body is not a JSON object")
	}
	s := &bodyState{keys: keys, vals: make(map[string]json.RawMessage, len(keys))}
	for i, k := range keys {
		s.vals[k] = vals[i]
	}
	if err := json.Unmarshal(s.vals[field], &s.arr); err != nil {
		return nil, fmt.Errorf("keyframe %s: %w", field, err)
	}
	return s, nil
}

func (s *bodyState) apply(d *Delta) error {
	if d.Keep > len(s.arr) {
		return fmt.Errorf("delta keeps %d of %d elements", d.Keep, len(s.arr))
	}
	vals := make(map[string]json.RawMessage, len(d.Keys))
	for _, k := range d.Keys {
		if k == d.Field {
			continue
		}
		v, ok := d.Set[k]
		if !ok {
			if v, ok = s.vals[k]; !ok {
				return fmt.Errorf("delta needs missing key %q", k)
			}
		}
		vals[k] = v
	}
	s.keys, s.vals = d.Keys, vals
	s.arr = append(s.arr[:d.Keep:d.Keep], d.Append...)
	return nil
}

func (s *bodyState) body(field string) json.RawMessage {
	b := []byte{'{'}
	for i, k := range s.keys {
		if i > 0 {
			b = append(b, ',')
		}
		kb, _ := json.Marshal(k)
		b = append(append(b, kb...), ':')
		if k != field {
			b = append(b, s.vals[k]...)
			continue
		}
		b = append(b, '[')
		for j, e := range s.arr {
			if j > 0 {
				b = append(b, ',')
			}
			b = append(b, e...)
		}
		b = append(b, ']')
	}
	return append(b, '}')
}

// fetchLines returns the wanted lines of a data object.
type fetchLines func(key string, lines []int) (map[int]json.RawMessage, error)

// expandOrKeep is expand, except that when an earlier record of the chain is
// gone (deleted object, lost batch) it returns raw as is, delta included, plus
// request_rebuild_error. The rest of the record is intact and still worth showing.
func expandOrKeep(key string, raw json.RawMessage, fetch fetchLines) (json.RawMessage, error) {
	out, err := expand(key, raw, fetch)
	if err == nil {
		return out, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, err
	}
	fields["request_rebuild_error"], _ = json.Marshal(err.Error())
	return json.Marshal(fields)
}

// expand returns raw (a record line read from data object key) with request_body
// rebuilt and request_delta removed. Records without a delta are returned as is.
func expand(key string, raw json.RawMessage, fetch fetchLines) (json.RawMessage, error) {
	var top struct {
		RequestDelta *Delta `json:"request_delta"`
	}
	if err := json.Unmarshal(raw, &top); err != nil || top.RequestDelta == nil {
		return raw, err
	}
	d := top.RequestDelta
	if len(d.Chain) == 0 {
		return nil, fmt.Errorf("delta with empty chain")
	}

	want := map[string][]int{}
	for _, ref := range d.Chain {
		k := ref.Key
		if k == "" {
			k = key
		}
		want[k] = append(want[k], ref.Line)
	}
	got := map[string]map[int]json.RawMessage{}
	for k, lines := range want {
		m, err := fetch(k, lines)
		if err != nil {
			return nil, fmt.Errorf("earlier record in %s: %w", k, err)
		}
		got[k] = m
	}
	line := func(ref Ref) (json.RawMessage, error) {
		k := ref.Key
		if k == "" {
			k = key
		}
		if l, ok := got[k][ref.Line]; ok {
			return l, nil
		}
		return nil, fmt.Errorf("chain record %s line %d not found", k, ref.Line)
	}

	type link struct {
		RequestBody  json.RawMessage `json:"request_body"`
		RequestDelta *Delta          `json:"request_delta"`
	}
	var s *bodyState
	for i, ref := range d.Chain {
		l, err := line(ref)
		if err != nil {
			return nil, err
		}
		var rec link
		if err := json.Unmarshal(l, &rec); err != nil {
			return nil, err
		}
		if i == 0 {
			if rec.RequestDelta != nil {
				return nil, fmt.Errorf("chain does not start at a keyframe")
			}
			if s, err = stateOf(rec.RequestBody, d.Field); err != nil {
				return nil, err
			}
			continue
		}
		if rec.RequestDelta == nil {
			return nil, fmt.Errorf("chain record %d is not a delta", i)
		}
		if err := s.apply(rec.RequestDelta); err != nil {
			return nil, err
		}
	}
	if err := s.apply(d); err != nil {
		return nil, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "request_delta")
	fields["request_body"] = s.body(d.Field)
	return json.Marshal(fields)
}
