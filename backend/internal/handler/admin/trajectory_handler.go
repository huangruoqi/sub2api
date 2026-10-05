package admin

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/trajectory"

	"github.com/gin-gonic/gin"
)

// Trajectory archive browser (fork feature, see railway/README.md). Plain
// funcs over trajectory.Default(), so no wire wiring.

// TrajectorySummary GET /admin/trajectories/summary
func TrajectorySummary(c *gin.Context) {
	rec := trajectory.Default()
	if rec == nil {
		response.Success(c, gin.H{"enabled": false})
		return
	}
	sum, err := rec.Summary(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusBadGateway, "list bucket: "+err.Error())
		return
	}
	response.Success(c, gin.H{"enabled": true, "summary": sum})
}

// TrajectoryRecords GET /admin/trajectories/records
//
// Window: start_time/end_time (RFC3339, default last hour, max 24h).
// Filters: request_id, session_id, model, path, user_id, api_key_id, account_id, group_id, status (ok|error|<code>).
// group_by (session|model|user|api_key|account|group|path|status) returns groups instead of items.
// order=asc lists oldest first (useful for reading one session in order).
func TrajectoryRecords(c *gin.Context) {
	rec := trajectory.Default()
	if rec == nil {
		response.NotFound(c, "trajectory archive is disabled")
		return
	}
	end, start := time.Now(), time.Now().Add(-time.Hour)
	for name, dst := range map[string]*time.Time{"start_time": &start, "end_time": &end} {
		if v := strings.TrimSpace(c.Query(name)); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				response.BadRequest(c, "Invalid "+name+", expect RFC3339")
				return
			}
			*dst = t
		}
	}
	if !end.After(start) || end.Sub(start) > trajectory.MaxQueryWindow {
		response.BadRequest(c, "time window must be positive and at most 24h")
		return
	}

	f := trajectory.Filter{
		RequestID: strings.TrimSpace(c.Query("request_id")),
		SessionID: strings.TrimSpace(c.Query("session_id")),
		Model:     strings.TrimSpace(c.Query("model")),
		Path:      strings.TrimSpace(c.Query("path")),
		Status:    strings.TrimSpace(c.Query("status")),
	}
	for name, dst := range map[string]*int64{"user_id": &f.UserID, "api_key_id": &f.APIKeyID, "account_id": &f.AccountID, "group_id": &f.GroupID} {
		if v := strings.TrimSpace(c.Query(name)); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				response.BadRequest(c, "Invalid "+name)
				return
			}
			*dst = id
		}
	}
	groupBy := strings.TrimSpace(c.Query("group_by"))
	field, ok := trajectory.GroupFields[groupBy]
	if groupBy != "" && !ok {
		response.BadRequest(c, "Invalid group_by")
		return
	}

	entries, err := rec.Index(c.Request.Context(), start, end)
	if err != nil {
		response.Error(c, http.StatusBadGateway, "read index: "+err.Error())
		return
	}
	matched := entries[:0]
	for i := range entries {
		if f.Match(&entries[i]) {
			matched = append(matched, entries[i])
		}
	}
	if c.Query("order") == "asc" {
		slices.Reverse(matched)
	}

	page, pageSize := response.ParsePagination(c)
	if groupBy != "" {
		groups := trajectory.GroupBy(matched, field)
		response.Paginated(c, pageOf(groups, page, pageSize), int64(len(groups)), page, pageSize)
		return
	}
	response.Paginated(c, pageOf(matched, page, pageSize), int64(len(matched)), page, pageSize)
}

// TrajectoryRecord GET /admin/trajectories/record?key=...&line=N returns one full record.
func TrajectoryRecord(c *gin.Context) {
	rec := trajectory.Default()
	if rec == nil {
		response.NotFound(c, "trajectory archive is disabled")
		return
	}
	line, err := strconv.Atoi(c.Query("line"))
	if err != nil {
		response.BadRequest(c, "Invalid line")
		return
	}
	raw, err := rec.ReadRecord(c.Request.Context(), c.Query("key"), line)
	if err != nil {
		response.NotFound(c, err.Error())
		return
	}
	response.Success(c, raw)
}

func pageOf[T any](items []T, page, pageSize int) []T {
	from := min((page-1)*pageSize, len(items))
	return items[from:min(from+pageSize, len(items))]
}
