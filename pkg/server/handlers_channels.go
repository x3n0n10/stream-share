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
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

const channelSearchLimit = 25

// searchChannels GET /api/internal/channels?q=... suggests live channels by
// name (or stream_id) for the health-check wizard's probe-channel picker, so
// an operator doesn't have to already know a raw Xtream stream ID. Reads only
// data already collected and persisted at startup (see warmChannelNameIndex
// and warmCategoryNameIndex) — this never calls the upstream provider.
func (c *Config) searchChannels(ctx *gin.Context) {
	query := strings.TrimSpace(ctx.Query("q"))
	results, err := c.db.SearchStreamNames(query, channelSearchLimit)
	if err != nil {
		utils.ErrorLog("Channel search failed: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Failed to search channels: " + err.Error(),
		})
		return
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: results})
}
