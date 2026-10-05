package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/trajectory"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// End to end: middleware -> recorder -> spool -> PUT to a fake S3 endpoint.
func TestTrajectoryMiddlewareArchivesRequestAndResponse(t *testing.T) {
	// Minimal path-style S3: PUT, GET object, ListObjectsV2.
	var mu sync.Mutex
	objects := map[string][]byte{}
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPut:
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
		case r.URL.Query().Get("list-type") == "2":
			prefix := r.URL.Path + "/" + r.URL.Query().Get("prefix")
			var keys []string
			for k := range objects {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			out := `<ListBucketResult><IsTruncated>false</IsTruncated>`
			for _, k := range keys {
				out += fmt.Sprintf("<Contents><Key>%s</Key><Size>%d</Size></Contents>", strings.TrimPrefix(k, "/b/"), len(objects[k]))
			}
			_, _ = io.WriteString(w, out+`</ListBucketResult>`)
		default:
			data, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(data)
		}
	}))
	defer s3srv.Close()
	cfg := config.TrajectoryConfig{
		Enabled: true, Endpoint: s3srv.URL, Region: "auto", Bucket: "b",
		AccessKeyID: "k", SecretAccessKey: "s", Prefix: "t/", ForcePathStyle: true,
		MaxBodyBytes: 1 << 20, SpoolDir: t.TempDir(),
	}

	require.NoError(t, trajectory.Init(cfg))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TrajectoryMiddleware())
	r.POST("/v1/messages", func(c *gin.Context) {
		got, _ := io.ReadAll(c.Request.Body)
		require.Contains(t, string(got), `"messages":[]`) // body still reaches the handler
		c.Set(opsModelKey, "m")
		c.Set(opsAccountIDKey, int64(7))
		c.Header("Content-Type", "text/event-stream")
		c.String(http.StatusOK, "data: a\n\n")
		c.Writer.Flush()
		c.String(http.StatusOK, "data: b\n\n")
	})

	sessionBody := `{"model":"m","messages":[],"metadata":{"user_id":"user_` + strings.Repeat("a", 64) + `_account__session_11111111-2222-3333-4444-555555555555"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(sessionBody))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Api-Key", "secret")
	req.Header.Set("Anthropic-Beta", "x")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, "data: a\n\ndata: b\n\n", resp.Body.String())

	trajectory.Close(context.Background()) // flush -> spool -> upload data + index

	mu.Lock()
	require.Len(t, objects, 2)
	mu.Unlock()

	// Read side, as the admin dashboard uses it.
	require.NoError(t, trajectory.Init(cfg))
	defer trajectory.Close(context.Background())
	tr := trajectory.Default()
	ctx := context.Background()

	sum, err := tr.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, sum.Records)
	require.Equal(t, 1, sum.Objects)

	entries, err := tr.Index(ctx, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	e := entries[0]
	require.Equal(t, "11111111-2222-3333-4444-555555555555", e.SessionID)
	require.True(t, trajectory.Filter{SessionID: e.SessionID, Status: "ok"}.Match(&e))
	require.False(t, trajectory.Filter{Status: "error"}.Match(&e))
	groups := trajectory.GroupBy(entries, trajectory.GroupFields["session"])
	require.Len(t, groups, 1)
	require.Equal(t, 1, groups[0].Count)
	require.Equal(t, []string{"m"}, groups[0].Models)

	raw, err := tr.ReadRecord(ctx, e.Key, e.Line)
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(raw, &rec))
	require.Equal(t, "m", rec["model"])
	require.EqualValues(t, 7, rec["account_id"])
	require.EqualValues(t, 200, rec["status"])
	reqBody, ok := rec["request_body"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "m", reqBody["model"])
	require.Equal(t, "data: a\n\ndata: b\n\n", rec["response_body"])
	headers, ok := rec["request_headers"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "x", headers["anthropic-beta"])
	require.NotContains(t, headers, "authorization")
	require.NotContains(t, headers, "x-api-key")

	_, err = tr.ReadRecord(ctx, "t/index/../x", 0)
	require.Error(t, err) // only data/ keys are readable
}
