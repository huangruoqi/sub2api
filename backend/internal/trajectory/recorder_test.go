package trajectory

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// A session that resends a growing conversation must compress to roughly its
// unique bytes, and both codecs must read back line for line.
func TestBatchRoundTrip(t *testing.T) {
	var history strings.Builder
	for i := 0; i < 2000; i++ { // ~100 KB of non-repeating text
		_, _ = history.WriteString(strings.Repeat(string(rune('a'+i%26)), i%7+1))
		_, _ = history.WriteString(" ")
		_, _ = history.WriteString(string(rune('0' + i*7919%10)))
	}
	b := newBatch(dataWindow)
	raw := 0
	for i := 0; i < 50; i++ {
		rec := &Record{Meta: Meta{RequestID: "r"}, RequestBody: Body{Data: []byte(`"` + history.String() + `"`)}}
		line, _ := json.Marshal(rec)
		raw += len(line) + 1
		if err := b.enc.Encode(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.close(); err != nil {
		t.Fatal(err)
	}
	if got := b.buf.Len(); got*20 > raw {
		t.Fatalf("repeated requests compressed %d -> %d, want >20x", raw, got)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("{\"a\":1}\n"))
	_ = zw.Close()

	for key, data := range map[string][]byte{"x" + ext: b.buf.Bytes(), "x.jsonl.gz": gz.Bytes()} {
		rc, err := decompress(key, io.NopCloser(bytes.NewReader(data)))
		if err != nil {
			t.Fatal(key, err)
		}
		out, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || bytes.Count(out, []byte("\n")) == 0 {
			t.Fatal(key, err, len(out))
		}
	}
}
