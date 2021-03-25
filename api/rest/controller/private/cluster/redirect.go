package cluster

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/private/cluster"
	"github.com/labstack/echo/v4"
)

func Redirect(c echo.Context) error {
	svc := cluster.Service()
	leaderAPIAddr := svc.LeaderAPIAddr()
	if leaderAPIAddr == "" {
		return c.JSON(http.StatusServiceUnavailable, "cannot reach leader")
	}

	return c.Redirect(
		http.StatusMovedPermanently,
		svc.FormRedirect(c.Request(), svc.LeaderAPIProto(), leaderAPIAddr),
	)
}
