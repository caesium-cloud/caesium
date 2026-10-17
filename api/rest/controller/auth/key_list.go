package auth

import (
	"encoding/json"
	"net/http"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
	"gorm.io/datatypes"
)

// ListKeys returns the API keys. A principal holding admin at "*" sees every
// key; a namespace admin (the collection's restricted case) sees only keys
// confined to namespaces where it holds admin, never a cluster-wide key.
func (ctrl *Controller) ListKeys(c *echo.Context) error {
	keys, err := ctrl.service.ListKeys()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to list api keys").Wrap(err)
	}
	if allowed, restricted := authz.AllowedNamespaces(c.Request().Context()); restricted {
		visible := make([]models.APIKey, 0, len(keys))
		for _, key := range keys {
			if iauth.KeyVisibleTo(key.Namespaces, allowed) {
				visible = append(visible, key)
			}
		}
		keys = visible
		authz.MarkFiltered(c)
	}
	for i := range keys {
		keys[i].Namespaces = listableNamespaces(keys[i].Namespaces)
	}
	return c.JSON(http.StatusOK, keys)
}

// listableNamespaces keeps a stored namespaces value serialisable: a value
// that is not JSON at all (it grants nothing) is listed as a JSON string of
// its raw text, so one corrupt row cannot fail the whole list and the cluster
// admin can still find the key to revoke it.
func listableNamespaces(raw datatypes.JSON) datatypes.JSON {
	if len(raw) == 0 || json.Valid(raw) {
		return raw
	}
	quoted, err := json.Marshal(string(raw))
	if err != nil {
		return nil
	}
	return datatypes.JSON(quoted)
}
