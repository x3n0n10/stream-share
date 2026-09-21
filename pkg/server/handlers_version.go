/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2026  x3n0n10
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/version"
)

// getVersion GET /api/internal/version — the release this instance is running,
// so a dashboard managing several instances can show it without having to
// inspect the container image. A build made without a stamped version reports
// "dev".
func (c *Config) getVersion(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data:    map[string]interface{}{"version": version.Version},
	})
}
