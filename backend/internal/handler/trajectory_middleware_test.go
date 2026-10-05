package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/trajectory"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// End to end: middleware -> recorder -> spool -> PUT to a fake S3 endpoint.
func TestTrajectoryMiddlewareArchivesRequestAndResponse(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		objects[r.URL.Path] = body
		mu.Unlock()
	}))
	defer s3srv.Close()

	require.NoError(t, trajectory.Init(config.TrajectoryConfig{
		Enabled: true, Endpoint: s3srv.URL, Region: "auto", Bucket: "b",
		AccessKeyID: "k", SecretAccessKey: "s", Prefix: "t/", ForcePathStyle: true,
		MaxBodyBytes: 1 << 20, SpoolDir: t.TempDir(),
	}))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TrajectoryMiddleware())
	r.POST("/v1/messages", func(c *gin.Context) {
		got, _ := io.ReadAll(c.Request.Body)
		require.JSONEq(t, `{"model":"m","messages":[]}`, string(got)) // body still reaches the handler
		c.Set(opsModelKey, "m")
		c.Set(opsAccountIDKey, int64(7))
		c.Header("Content-Type", "text/event-stream")
		c.String(http.StatusOK, "data: a\n\n")
		c.Writer.Flush()
		c.String(http.StatusOK, "data: b\n\n")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Api-Key", "secret")
	req.Header.Set("Anthropic-Beta", "x")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, "data: a\n\ndata: b\n\n", resp.Body.String())

	trajectory.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, objects, 1)
	for key, data := range objects {
		require.True(t, strings.HasPrefix(key, "/b/t/"), key)
		zr, err := gzip.NewReader(bytes.NewReader(data))
		require.NoError(t, err)
		line, err := io.ReadAll(zr)
		require.NoError(t, err)
		var rec map[string]any
		require.NoError(t, json.Unmarshal(line, &rec))
		require.Equal(t, "m", rec["model"])
		require.EqualValues(t, 7, rec["account_id"])
		require.EqualValues(t, 200, rec["status"])
		require.Equal(t, map[string]any{"model": "m", "messages": []any{}}, rec["request_body"])
		require.Equal(t, "data: a\n\ndata: b\n\n", rec["response_body"])
		headers := rec["request_headers"].(map[string]any)
		require.Equal(t, "x", headers["anthropic-beta"])
		require.NotContains(t, headers, "authorization")
		require.NotContains(t, headers, "x-api-key")
	}
}
