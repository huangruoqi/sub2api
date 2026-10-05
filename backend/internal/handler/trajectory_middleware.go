package handler

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/trajectory"

	"github.com/gin-gonic/gin"
)

// TrajectoryMiddleware archives the full request and response body of every
// authenticated gateway POST (see internal/trajectory). Mount it after API key
// auth. It is a no-op when the archive is disabled.
func TrajectoryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		rec := trajectory.Default()
		if rec == nil || c.Request.Method != http.MethodPost || c.IsWebsocket() {
			// ponytail: websocket frames (/live, realtime) are not captured.
			c.Next()
			return
		}

		// Read once (decoding Content-Encoding) and put it back as a PrereadBody,
		// the same thing groupModelAllowlist/compositeTarget already do, so
		// downstream reads are zero-copy and see identical bytes.
		reqBody, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		if err != nil {
			// Downstream turns this read error into its usual 400/413.
			c.Request.Body = errBody{err}
			c.Next()
			return
		}
		requestmodel.ResetRequestBody(c.Request, reqBody)

		w := &trajectoryWriter{ResponseWriter: c.Writer, limit: rec.MaxBodyBytes()}
		c.Writer = w
		start := time.Now()
		defer func() {
			if c.Writer == w {
				c.Writer = w.ResponseWriter
			}
		}()

		c.Next()

		r := &trajectory.Record{
			Meta: trajectory.Meta{
				Time:          start.UTC(),
				SessionID:     service.TrajectorySessionID(c, reqBody),
				Method:        c.Request.Method,
				Path:          c.Request.URL.Path,
				Status:        w.Status(),
				LatencyMs:     time.Since(start).Milliseconds(),
				Model:         c.GetString(opsModelKey),
				UpstreamModel: c.GetString(opsUpstreamModelKey),
				Stream:        c.GetBool(opsStreamKey),
				ResponseType:  w.Header().Get("Content-Type"),
			},
			RequestHeaders: trajectoryHeaders(c.Request.Header),
			RequestBody:    truncatedBody(reqBody, rec.MaxBodyBytes()),
			ResponseBody:   trajectory.Body{Data: w.buf.Bytes(), Truncated: w.truncated},
		}
		r.RequestID, _ = c.Request.Context().Value(ctxkey.RequestID).(string)
		r.ClientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		if v, ok := c.Get(opsAccountIDKey); ok {
			r.AccountID, _ = v.(int64)
		}
		if apiKey := getOpsAPIKey(c); apiKey != nil {
			r.APIKeyID, r.UserID = apiKey.ID, apiKey.UserID
			if apiKey.GroupID != nil {
				r.GroupID = *apiKey.GroupID
			}
		}
		rec.Record(r)
	}
}

type trajectoryWriter struct {
	gin.ResponseWriter
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *trajectoryWriter) capture(b []byte) {
	if w.limit > 0 && w.buf.Len()+len(b) > w.limit {
		b = b[:max(0, w.limit-w.buf.Len())]
		w.truncated = true
	}
	_, _ = w.buf.Write(b) // bytes.Buffer.Write never returns an error
}

func (w *trajectoryWriter) Write(b []byte) (int, error) {
	w.capture(b)
	return w.ResponseWriter.Write(b)
}

func (w *trajectoryWriter) WriteString(s string) (int, error) {
	w.capture([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

func truncatedBody(b []byte, limit int) trajectory.Body {
	if limit > 0 && len(b) > limit {
		return trajectory.Body{Data: b[:limit], Truncated: true}
	}
	return trajectory.Body{Data: b}
}

// trajectoryHeaders keeps request headers (anthropic-beta, user-agent, ...)
// minus anything that looks like a credential.
func trajectoryHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "auth") || strings.Contains(lk, "key") || strings.Contains(lk, "token") ||
			strings.Contains(lk, "cookie") || strings.Contains(lk, "secret") {
			continue
		}
		out[lk] = strings.Join(v, ", ")
	}
	return out
}

type errBody struct{ err error }

func (e errBody) Read([]byte) (int, error) { return 0, e.err }
func (e errBody) Close() error             { return nil }
