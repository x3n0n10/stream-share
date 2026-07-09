/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
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
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// sinceFromHours converts an hours window into a lower-bound time.
// hours <= 0 yields the zero time (no lower bound / all-time).
func sinceFromHours(hours int) time.Time {
	if hours > 0 {
		return time.Now().Add(-time.Duration(hours) * time.Hour)
	}
	return time.Time{}
}

// getHistorySummary GET /api/internal/history?hours=N — per-user aggregates.
func (c *Config) getHistorySummary(ctx *gin.Context) {
	if c.db == nil {
		utils.ErrorLog("Database is nil in getHistorySummary")
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Database not initialized",
		})
		return
	}

	hours, _ := strconv.Atoi(ctx.Query("hours"))
	since := sinceFromHours(hours)

	summaries, err := c.db.GetHistorySummary(since)
	if err != nil {
		utils.ErrorLog("Failed to get history summary: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to get history summary: %v", err),
		})
		return
	}

	type item struct {
		Username    string    `json:"username"`
		TotalCount  int       `json:"total_count"`
		LiveCount   int       `json:"live_count"`
		VODCount    int       `json:"vod_count"`
		TotalSec    int64     `json:"total_sec"`
		LastWatched time.Time `json:"last_watched"`
	}
	summaryItems := make([]item, 0, len(summaries))
	for _, s := range summaries {
		summaryItems = append(summaryItems, item{
			Username:    s.Username,
			TotalCount:  s.TotalCount,
			LiveCount:   s.LiveCount,
			VODCount:    s.VODCount,
			TotalSec:    s.TotalSec,
			LastWatched: s.LastWatched,
		})
	}

	var b strings.Builder
	if len(summaryItems) == 0 {
		b.WriteString("No watch history in this period.")
	} else {
		for _, it := range summaryItems {
			watched := utils.HumanDuration(time.Duration(it.TotalSec) * time.Second)
			last := "never"
			if !it.LastWatched.IsZero() {
				last = utils.HumanDuration(time.Since(it.LastWatched)) + " ago"
			}
			fmt.Fprintf(&b, "- %s: %d stream(s) (%d live, %d VOD) — %s watched, last %s\n",
				it.Username, it.TotalCount, it.LiveCount, it.VODCount, watched, last,
			)
		}
	}

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"summary":    summaryItems,
			"text":       b.String(),
			"hours":      hours,
			"user_count": len(summaryItems),
		},
	})
}

// getUserHistory GET /api/internal/history/:username?hours=N — one user's rows.
func (c *Config) getUserHistory(ctx *gin.Context) {
	if c.db == nil {
		utils.ErrorLog("Database is nil in getUserHistory")
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Database not initialized",
		})
		return
	}

	username := ctx.Param("username")
	if username == "" {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{
			Success: false,
			Error:   "username is required",
		})
		return
	}

	hours, _ := strconv.Atoi(ctx.Query("hours"))
	since := sinceFromHours(hours)

	const limit = 50
	entries, err := c.db.GetUserHistory(username, since, limit)
	if err != nil {
		utils.ErrorLog("Failed to get user history: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to get user history: %v", err),
		})
		return
	}

	type item struct {
		StreamID    string    `json:"stream_id"`
		StreamType  string    `json:"stream_type"`
		StreamTitle string    `json:"stream_title"`
		StartTime   time.Time `json:"start_time"`
		DurationSec int64     `json:"duration_sec"`
	}
	entryItems := make([]item, 0, len(entries))
	for _, e := range entries {
		entryItems = append(entryItems, item{
			StreamID:    e.StreamID,
			StreamType:  e.StreamType,
			StreamTitle: e.StreamTitle,
			StartTime:   e.StartTime,
			DurationSec: e.DurationSec,
		})
	}

	var b strings.Builder
	if len(entryItems) == 0 {
		fmt.Fprintf(&b, "No watch history for %s in this period.", username)
	} else {
		for _, it := range entryItems {
			label := strings.TrimSpace(it.StreamTitle)
			if label == "" {
				label = it.StreamID
			}
			dur := utils.HumanDuration(time.Duration(it.DurationSec) * time.Second)
			fmt.Fprintf(&b, "- %s [%s] — %s (%s)\n",
				label, it.StreamType, dur, it.StartTime.Format("2006-01-02 15:04"),
			)
		}
	}

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"username": username,
			"entries":  entryItems,
			"text":     b.String(),
			"hours":    hours,
			"count":    len(entryItems),
		},
	})
}
