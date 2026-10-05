package service

import (
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// TrajectorySessionID returns the conversation id the client sent explicitly
// (Claude Code metadata.user_id session, OpenAI/Codex session headers or
// prompt_cache_key), so the trajectory archive can group a session's requests.
// Unlike the sticky-session hash it never falls back to hashing content.
func TrajectorySessionID(c *gin.Context, body []byte) string {
	if raw := gjson.GetBytes(body, "metadata.user_id").String(); raw != "" {
		if uid := ParseMetadataUserID(raw); uid != nil && uid.SessionID != "" {
			return uid.SessionID
		}
	}
	return explicitOpenAIRequestSessionID(c, body)
}
