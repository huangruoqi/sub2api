package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"

	"github.com/gin-gonic/gin"
)

// registerTrajectoryRoutes mounts the trajectory archive browser (fork feature).
func registerTrajectoryRoutes(adminGroup *gin.RouterGroup) {
	g := adminGroup.Group("/trajectories")
	g.GET("/summary", admin.TrajectorySummary)
	g.GET("/records", admin.TrajectoryRecords)
	g.GET("/record", admin.TrajectoryRecord)
}
