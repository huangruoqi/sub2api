// Package trajectory archives every gateway request/response pair as zstd JSONL
// in an S3-compatible bucket (Railway Bucket in production).
//
// Records are queued and written by one background goroutine. Recording never blocks
// a user request and never fails one: if the queue is full the record is
// dropped and counted.
package trajectory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/klauspost/compress/zstd"
)

// ponytail: fixed knobs; promote to config if a week of real traffic says otherwise.
const (
	queueSize     = 4096
	flushInterval = time.Minute
	flushBytes    = 64 << 20 // compressed
	// Agent clients resend the whole conversation every turn. A window this big
	// lets zstd turn a session's repeated prefix into back-references, which gzip's
	// 32 KB window can't do. Costs ~window bytes of RAM in the writer and in each reader.
	dataWindow = 128 << 20
	ext        = ".jsonl.zst"
)

// Meta is everything about a request except headers and bodies. It is
// stored in both the data line and the index line.
type Meta struct {
	Time            time.Time `json:"ts"`
	RequestID       string    `json:"request_id,omitempty"`
	ClientRequestID string    `json:"client_request_id,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	Method          string    `json:"method"`
	Path            string    `json:"path"`
	Status          int       `json:"status"`
	LatencyMs       int64     `json:"latency_ms"`
	UserID          int64     `json:"user_id,omitempty"`
	APIKeyID        int64     `json:"api_key_id,omitempty"`
	GroupID         int64     `json:"group_id,omitempty"`
	AccountID       int64     `json:"account_id,omitempty"`
	Model           string    `json:"model,omitempty"`
	UpstreamModel   string    `json:"upstream_model,omitempty"`
	Stream          bool      `json:"stream"`
	ResponseType    string    `json:"response_content_type,omitempty"`
}

// Record is one line of a data object.
type Record struct {
	Meta
	RequestHeaders map[string]string `json:"request_headers,omitempty"`
	RequestBody    Body              `json:"request_body"`
	ResponseBody   Body              `json:"response_body"`
}

// IndexEntry is one line of an index object: Meta plus where the full record
// lives (Line in the sibling data object; Key is filled in on read).
type IndexEntry struct {
	Meta
	Line          int    `json:"line"`
	RequestBytes  int    `json:"request_bytes"`
	ResponseBytes int    `json:"response_bytes"`
	Key           string `json:"key,omitempty"`
}

// Body is stored inline as JSON when it is JSON, as a string when it is UTF-8 text
// (SSE streams), otherwise base64 (encoding/json's []byte encoding).
type Body struct {
	Data      []byte
	Truncated bool
}

func (b Body) MarshalJSON() ([]byte, error) {
	var v any
	switch {
	case len(b.Data) == 0:
		v = nil
	case !b.Truncated && json.Valid(b.Data):
		v = json.RawMessage(b.Data)
	case utf8.Valid(b.Data):
		v = string(b.Data)
	default:
		v = map[string]any{"base64": b.Data}
	}
	if b.Truncated {
		return json.Marshal(map[string]any{"truncated": true, "data": v})
	}
	return json.Marshal(v)
}

type Recorder struct {
	client   *s3.Client
	bucket   string
	prefix   string
	spoolDir string
	host     string
	maxBody  int

	ch      chan *Record
	done    chan struct{}
	closing sync.Once
	dropped atomic.Int64

	summaryMu sync.Mutex
	summary   *Summary
	indexMu   sync.Mutex
	index     map[string][]IndexEntry // immutable objects, cached by key
}

var std atomic.Pointer[Recorder]

// Default returns the process recorder, or nil when archiving is disabled.
func Default() *Recorder { return std.Load() }

// Init starts the process recorder when cfg.Enabled. ponytail: process global so
// it skips the wire graph; it only has to be reachable from one middleware and main.
func Init(cfg config.TrajectoryConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return fmt.Errorf("trajectory: enabled but bucket/access_key_id/secret_access_key missing")
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0o755); err != nil {
		return fmt.Errorf("trajectory: spool dir: %w", err)
	}
	host := os.Getenv("RAILWAY_REPLICA_ID")
	if host == "" {
		host, _ = os.Hostname()
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	client := s3.New(s3.Options{
		Region:                     region,
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		BaseEndpoint:               nilIfEmpty(cfg.Endpoint),
		UsePathStyle:               cfg.ForcePathStyle,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	}, func(o *s3.Options) {
		// Same S3-compat tweak as repository.newS3Client (R2/OSS/Railway).
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	})
	r := &Recorder{
		client:   client,
		bucket:   cfg.Bucket,
		prefix:   cfg.Prefix,
		spoolDir: cfg.SpoolDir,
		host:     host,
		maxBody:  cfg.MaxBodyBytes,
		ch:       make(chan *Record, queueSize),
		done:     make(chan struct{}),
		index:    map[string][]IndexEntry{},
	}
	go r.run()
	std.Store(r)
	slog.Info("trajectory archive enabled", "bucket", cfg.Bucket, "prefix", cfg.Prefix, "spool_dir", cfg.SpoolDir)
	return nil
}

// Close flushes the pending batch and stops the recorder.
func Close(ctx context.Context) {
	r := std.Swap(nil)
	if r == nil {
		return
	}
	r.closing.Do(func() { close(r.ch) })
	select {
	case <-r.done:
	case <-ctx.Done():
		slog.Warn("trajectory: shutdown timed out; unsent batches stay in the spool dir and upload on next start")
	}
}

// MaxBodyBytes is the per-body capture limit (<=0 means unlimited).
func (r *Recorder) MaxBodyBytes() int { return r.maxBody }

// Record enqueues rec without blocking.
func (r *Recorder) Record(rec *Record) {
	defer func() { _ = recover() }() // send on channel closed by a racing Close
	select {
	case r.ch <- rec:
	default:
		if n := r.dropped.Add(1); n == 1 || n%1000 == 0 {
			slog.Warn("trajectory: queue full, dropping records", "dropped_total", n)
		}
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	r.uploadSpool() // leftovers from a previous process

	data, index := newBatch(dataWindow), newBatch(0)
	n := 0
	flush := func() {
		if n == 0 {
			return
		}
		if err := data.close(); err == nil {
			// Index goes to the spool only if its data made it.
			now := time.Now().UTC()
			name := fmt.Sprintf("%s/%s-%d-n%d"+ext, now.Format("2006/01/02/15"), r.host, now.UnixNano(), n)
			if r.spool(r.prefix+"data/"+name, data.buf.Bytes()) {
				if err := index.close(); err == nil {
					r.spool(r.prefix+"index/"+name, index.buf.Bytes())
				}
			}
		} else {
			slog.Error("trajectory: zstd close failed, batch lost", "error", err, "records", n)
		}
		data.reset()
		index.reset()
		n = 0
		r.uploadSpool()
	}

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case rec, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			if err := data.enc.Encode(rec); err != nil {
				slog.Error("trajectory: encode failed", "error", err, "request_id", rec.RequestID)
				continue
			}
			_ = index.enc.Encode(IndexEntry{
				Meta: rec.Meta, Line: n,
				RequestBytes: len(rec.RequestBody.Data), ResponseBytes: len(rec.ResponseBody.Data),
			})
			n++
			if data.buf.Len() >= flushBytes {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

type batch struct {
	buf bytes.Buffer
	zw  *zstd.Encoder
	enc *json.Encoder
}

// newBatch uses zstd's default window when window is 0.
func newBatch(window int) *batch {
	b := &batch{}
	opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(1)}
	if window > 0 {
		opts = append(opts, zstd.WithWindowSize(window))
	}
	b.zw, _ = zstd.NewWriter(&b.buf, opts...) // only fails on invalid options
	b.enc = json.NewEncoder(b.zw)
	return b
}

func (b *batch) close() error { return b.zw.Close() }
func (b *batch) reset()       { b.buf.Reset(); b.zw.Reset(&b.buf) }

// spool writes a finished batch to disk first, so an upload outage or restart
// doesn't lose it. The file path under spoolDir is the object key.
func (r *Recorder) spool(key string, data []byte) bool {
	path := filepath.Join(r.spoolDir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if err = os.WriteFile(path, data, 0o644); err == nil {
			return true
		}
	}
	// Disk failed; try a direct upload rather than drop the batch.
	if err := r.put(key, data); err != nil {
		slog.Error("trajectory: spool and upload both failed, batch lost", "error", err, "key", key)
		return false
	}
	return true
}

func (r *Recorder) uploadSpool() {
	_ = filepath.WalkDir(r.spoolDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(r.spoolDir, path)
		if err != nil {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if err := r.put(filepath.ToSlash(rel), data); err != nil {
			slog.Warn("trajectory: upload failed, will retry next flush", "error", err, "key", rel)
			return fs.SkipAll
		}
		_ = os.Remove(path)
		return nil
	})
}

func (r *Recorder) put(key string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(r.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType(key)),
	})
	return err
}

func contentType(key string) string {
	if strings.HasSuffix(key, ".gz") {
		return "application/gzip" // spool left over from a pre-zstd build
	}
	return "application/zstd"
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
