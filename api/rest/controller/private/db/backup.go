package db

import (
	"net/http"
	"strconv"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/labstack/echo/v4"
)

func Backup(c echo.Context) error {
	req := db.BackupRequest{Writer: c.Response().Writer}

	fmt, _ := strconv.ParseInt(c.Param("format"), 10, 32)
	req.Format = store.BackupFormat(fmt)
	req.LeaderOnly, _ = strconv.ParseBool(c.Param("leader_only"))

	if err := db.Service().Backup(&req); err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	return c.JSON(http.StatusNoContent, nil)
}
