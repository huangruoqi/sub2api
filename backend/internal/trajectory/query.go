package trajectory

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/errgroup"
)

const (
	summaryTTL       = 2 * time.Minute
	MaxQueryWindow   = 24 * time.Hour
	indexCacheMax    = 500_000 // entries; ponytail: reset-on-full, LRU if dashboards get busy
	indexFetchWorker = 16
)

// DaySummary aggregates data objects per UTC day, from object listing only.
type DaySummary struct {
	Day     string `json:"day"`
	Objects int    `json:"objects"`
	Records int64  `json:"records"`
	Bytes   int64  `json:"bytes"`
}

type Summary struct {
	Objects     int          `json:"objects"`
	Records     int64        `json:"records"`
	Bytes       int64        `json:"bytes"`
	Days        []DaySummary `json:"days"` // newest first
	SpoolFiles  int          `json:"spool_files"`
	Dropped     int64        `json:"dropped"`
	QueueLength int          `json:"queue_length"`
	Bucket      string       `json:"bucket"`
	Prefix      string       `json:"prefix"`
	GeneratedAt time.Time    `json:"generated_at"`
}

// data/YYYY/MM/DD/HH/<host>-<unixnano>-n<count>.jsonl.gz
var dataKeyRe = regexp.MustCompile(`data/(\d{4})/(\d{2})/(\d{2})/\d{2}/[^/]*-n(\d+)\.jsonl\.gz$`)

// Summary lists the whole data/ prefix. ponytail: full listing cached for
// summaryTTL; ~1 list call per 1000 batches (≈ per replica-day). Keep a running
// total object if this ever gets slow.
func (r *Recorder) Summary(ctx context.Context) (*Summary, error) {
	r.summaryMu.Lock()
	defer r.summaryMu.Unlock()
	if r.summary != nil && time.Since(r.summary.GeneratedAt) < summaryTTL {
		return r.withLive(*r.summary), nil
	}
	days := map[string]*DaySummary{}
	sum := Summary{Bucket: r.bucket, Prefix: r.prefix, GeneratedAt: time.Now()}
	err := r.list(ctx, r.prefix+"data/", func(key string, size int64) {
		m := dataKeyRe.FindStringSubmatch(key)
		if m == nil {
			return
		}
		day := m[1] + "-" + m[2] + "-" + m[3]
		d := days[day]
		if d == nil {
			d = &DaySummary{Day: day}
			days[day] = d
		}
		n, _ := strconv.ParseInt(m[4], 10, 64)
		d.Objects++
		d.Records += n
		d.Bytes += size
		sum.Objects++
		sum.Records += n
		sum.Bytes += size
	})
	if err != nil {
		return nil, err
	}
	for _, d := range days {
		sum.Days = append(sum.Days, *d)
	}
	sort.Slice(sum.Days, func(i, j int) bool { return sum.Days[i].Day > sum.Days[j].Day })
	r.summary = &sum
	return r.withLive(sum), nil
}

// withLive fills in fields that must not be cached.
func (r *Recorder) withLive(s Summary) *Summary {
	s.Dropped = r.dropped.Load()
	s.QueueLength = len(r.ch)
	_ = filepath.WalkDir(r.spoolDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, err := filepath.Rel(r.spoolDir, path); err == nil && strings.HasPrefix(filepath.ToSlash(rel), r.prefix+"data/") {
			s.SpoolFiles++
		}
		return nil
	})
	return &s
}

// Index returns every index entry with start <= ts <= end, newest first.
func (r *Recorder) Index(ctx context.Context, start, end time.Time) ([]IndexEntry, error) {
	if end.Sub(start) > MaxQueryWindow {
		return nil, fmt.Errorf("time window exceeds %s", MaxQueryWindow)
	}
	var keys []string
	for h := start.UTC().Truncate(time.Hour); !h.After(end); h = h.Add(time.Hour) {
		if err := r.list(ctx, r.prefix+"index/"+h.Format("2006/01/02/15")+"/", func(key string, _ int64) {
			keys = append(keys, key)
		}); err != nil {
			return nil, err
		}
	}

	results := make([][]IndexEntry, len(keys))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(indexFetchWorker)
	for i, key := range keys {
		g.Go(func() error {
			entries, err := r.readIndex(gctx, key)
			results[i] = entries
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var out []IndexEntry
	for _, entries := range results {
		for _, e := range entries {
			if !e.Time.Before(start) && !e.Time.After(end) {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

func (r *Recorder) readIndex(ctx context.Context, key string) ([]IndexEntry, error) {
	r.indexMu.Lock()
	cached, ok := r.index[key]
	r.indexMu.Unlock()
	if ok {
		return cached, nil
	}

	body, err := r.get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	dataKey := strings.Replace(key, r.prefix+"index/", r.prefix+"data/", 1)
	var entries []IndexEntry
	dec := json.NewDecoder(body)
	for {
		var e IndexEntry
		if err := dec.Decode(&e); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		e.Key = dataKey
		entries = append(entries, e)
	}

	r.indexMu.Lock()
	if len(r.index) > indexCacheMax/50 { // ~50 entries per batch on average
		r.index = map[string][]IndexEntry{}
	}
	r.index[key] = entries
	r.indexMu.Unlock()
	return entries, nil
}

// ReadRecord returns line `line` of data object `key` as raw JSON.
func (r *Recorder) ReadRecord(ctx context.Context, key string, line int) (json.RawMessage, error) {
	if !strings.HasPrefix(key, r.prefix+"data/") || strings.Contains(key, "..") || line < 0 {
		return nil, fmt.Errorf("invalid record reference")
	}
	body, err := r.get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	br := bufio.NewReaderSize(body, 1<<20)
	for i := 0; ; i++ {
		b, err := br.ReadBytes('\n')
		if i == line && len(b) > 0 {
			return json.RawMessage(b), nil
		}
		if err != nil {
			return nil, fmt.Errorf("line %d not found in %s", line, key)
		}
	}
}

// get returns the decompressed object body.
func (r *Recorder) get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(out.Body)
	if err != nil {
		_ = out.Body.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{zr, out.Body}, nil
}

func (r *Recorder) list(ctx context.Context, prefix string, fn func(key string, size int64)) error {
	p := s3.NewListObjectsV2Paginator(r.client, &s3.ListObjectsV2Input{Bucket: aws.String(r.bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			fn(aws.ToString(o.Key), aws.ToInt64(o.Size))
		}
	}
	return nil
}

// Filter narrows index entries. Zero values mean "any".
type Filter struct {
	RequestID string
	SessionID string
	Model     string // substring, case-insensitive
	Path      string // substring
	UserID    int64
	APIKeyID  int64
	AccountID int64
	GroupID   int64
	Status    string // "", "ok" (<400), "error" (>=400), or an exact code
}

func (f Filter) Match(e *IndexEntry) bool {
	switch {
	case f.RequestID != "" && e.RequestID != f.RequestID && e.ClientRequestID != f.RequestID:
	case f.SessionID != "" && e.SessionID != f.SessionID:
	case f.Model != "" && !strings.Contains(strings.ToLower(e.Model), strings.ToLower(f.Model)):
	case f.Path != "" && !strings.Contains(e.Path, f.Path):
	case f.UserID != 0 && e.UserID != f.UserID:
	case f.APIKeyID != 0 && e.APIKeyID != f.APIKeyID:
	case f.AccountID != 0 && e.AccountID != f.AccountID:
	case f.GroupID != 0 && e.GroupID != f.GroupID:
	case f.Status == "ok" && e.Status >= 400:
	case f.Status == "error" && e.Status < 400:
	case f.Status != "" && f.Status != "ok" && f.Status != "error" && strconv.Itoa(e.Status) != f.Status:
	default:
		return true
	}
	return false
}

// Group is one bucket of a group-by over index entries.
type Group struct {
	Key           string    `json:"key"`
	Count         int       `json:"count"`
	Errors        int       `json:"errors"`
	AvgLatencyMs  int64     `json:"avg_latency_ms"`
	RequestBytes  int64     `json:"request_bytes"`
	ResponseBytes int64     `json:"response_bytes"`
	First         time.Time `json:"first"`
	Last          time.Time `json:"last"`
	Models        []string  `json:"models"`
	UserID        int64     `json:"user_id,omitempty"` // of the latest request
}

var GroupFields = map[string]func(*IndexEntry) string{
	"session": func(e *IndexEntry) string { return e.SessionID },
	"model":   func(e *IndexEntry) string { return e.Model },
	"user":    func(e *IndexEntry) string { return idKey(e.UserID) },
	"api_key": func(e *IndexEntry) string { return idKey(e.APIKeyID) },
	"account": func(e *IndexEntry) string { return idKey(e.AccountID) },
	"group":   func(e *IndexEntry) string { return idKey(e.GroupID) },
	"path":    func(e *IndexEntry) string { return e.Path },
	"status":  func(e *IndexEntry) string { return strconv.Itoa(e.Status) },
}

func idKey(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// GroupBy buckets entries (assumed newest first) and sorts groups by most
// recent activity. Entries with an empty key are grouped under "".
func GroupBy(entries []IndexEntry, field func(*IndexEntry) string) []Group {
	idx := map[string]int{}
	var groups []Group
	var latency []int64
	for i := range entries {
		e := &entries[i]
		k := field(e)
		gi, ok := idx[k]
		if !ok {
			gi = len(groups)
			idx[k] = gi
			groups = append(groups, Group{Key: k, Last: e.Time, First: e.Time, UserID: e.UserID})
			latency = append(latency, 0)
		}
		g := &groups[gi]
		g.Count++
		if e.Status >= 400 {
			g.Errors++
		}
		latency[gi] += e.LatencyMs
		g.RequestBytes += int64(e.RequestBytes)
		g.ResponseBytes += int64(e.ResponseBytes)
		if e.Time.Before(g.First) {
			g.First = e.Time
		}
		if e.Time.After(g.Last) {
			g.Last = e.Time
		}
		if e.Model != "" && len(g.Models) < 5 && !slices.Contains(g.Models, e.Model) {
			g.Models = append(g.Models, e.Model)
		}
	}
	for i := range groups {
		groups[i].AvgLatencyMs = latency[i] / int64(groups[i].Count)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Last.After(groups[j].Last) })
	return groups
}
