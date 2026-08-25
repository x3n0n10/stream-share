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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/config"
)

func seriesTestConfig(baseURL string) *Config {
	return &Config{
		ProxyConfig: &config.ProxyConfig{
			XtreamBaseURL:  baseURL,
			XtreamUser:     "u",
			XtreamPassword: "p",
		},
	}
}

// resetSeriesCaches clears the package-level caches fetchSeriesEpisodeTitle
// reads/writes, so tests don't leak state into each other.
func resetSeriesCaches() {
	seriesListMu.Lock()
	seriesListCache = nil
	seriesListAt = time.Time{}
	seriesScanned = map[string]bool{}
	seriesListMu.Unlock()

	vodNameMu.Lock()
	vodNameIndex = map[string]string{}
	vodNameMu.Unlock()
}

// TestFetchSeriesEpisodeTitle_findsAndCachesSiblings is the regression test for
// series title resolution: get_vod_info can't resolve an episode id, so this
// crawls get_series + get_series_info instead. It also checks the crawl's key
// efficiency property — every episode seen while scanning a series (not just
// the requested one) gets cached, so a second episode from the same show
// resolves without another crawl.
func TestFetchSeriesEpisodeTitle_findsAndCachesSiblings(t *testing.T) {
	resetSeriesCaches()
	defer resetSeriesCaches()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "get_series":
			_, _ = w.Write([]byte(`[{"series_id":"10","name":"Show One"},{"series_id":"20","name":"Show Two"}]`))
		case "get_series_info":
			switch r.URL.Query().Get("series_id") {
			case "10":
				_, _ = w.Write([]byte(`{"episodes":{"1":[
					{"id":"501","title":"Pilot","episode_num":1},
					{"id":"502","title":"Second Episode","episode_num":2}
				]}}`))
			case "20":
				_, _ = w.Write([]byte(`{"episodes":{"1":[{"id":"900","title":"Other Show Ep","episode_num":1}]}}`))
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := seriesTestConfig(srv.URL)

	title, found, exhausted := c.fetchSeriesEpisodeTitle("502")
	if !found {
		t.Fatalf("expected episode 502 to resolve (exhausted=%v)", exhausted)
	}
	if want := "Show One S01E02 — Second Episode"; title != want {
		t.Fatalf("title = %q, want %q", title, want)
	}

	// The sibling episode (501, scanned as part of the same series) should
	// now be cached locally too, without needing another crawl.
	if name, ok := vodNameCached("501"); !ok || name != "Show One S01E01 — Pilot" {
		t.Fatalf("sibling episode 501 not cached from the same crawl: got (%q, %v)", name, ok)
	}
}

// TestFetchSeriesEpisodeTitle_notFound covers an id that isn't in the
// catalogue at all: the crawl must exhaust the series list and report a
// confirmed miss (exhausted=true) rather than hang or error.
func TestFetchSeriesEpisodeTitle_notFound(t *testing.T) {
	resetSeriesCaches()
	defer resetSeriesCaches()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "get_series":
			_, _ = w.Write([]byte(`[{"series_id":"10","name":"Show One"}]`))
		case "get_series_info":
			_, _ = w.Write([]byte(`{"episodes":{"1":[{"id":"501","title":"Pilot","episode_num":1}]}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := seriesTestConfig(srv.URL)
	_, found, exhausted := c.fetchSeriesEpisodeTitle("999999")
	if found {
		t.Fatal("expected no match for an id absent from the catalogue")
	}
	if !exhausted {
		t.Fatal("expected a full-catalogue miss to be reported as exhausted")
	}
}

// TestFetchSeriesEpisodeTitle_budgetCutoffIsNotExhausted is the regression
// test for the production bug: the first crawl for a large/slow catalogue
// can run out of its time budget before scanning anything, and that must NOT
// be reported as exhausted (resolveTitleAtStart only memoises an exhausted
// miss — treating a budget cutoff as final would pin the raw stream id as
// the title for titleMissRetryAfter, even though a retry moments later,
// resuming from the same series-scanned progress, would have found it).
func TestFetchSeriesEpisodeTitle_budgetCutoffIsNotExhausted(t *testing.T) {
	resetSeriesCaches()
	defer resetSeriesCaches()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "get_series":
			_, _ = w.Write([]byte(`[{"series_id":"10","name":"Show One"}]`))
		case "get_series_info":
			_, _ = w.Write([]byte(`{"episodes":{"1":[{"id":"900","title":"Target Episode","episode_num":1}]}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := seriesTestConfig(srv.URL)

	// An already-elapsed budget: the deadline check before the very first
	// get_series_info call fails immediately, so nothing gets scanned.
	orig := seriesEpisodeCrawlBudget
	seriesEpisodeCrawlBudget = -1 * time.Millisecond
	_, found, exhausted := c.fetchSeriesEpisodeTitle("900")
	seriesEpisodeCrawlBudget = orig
	if found {
		t.Fatal("did not expect a match with a budget too small to make any request")
	}
	if exhausted {
		t.Fatal("a budget cutoff must not be reported as an exhausted (confirmed) miss")
	}

	// A later call — e.g. the next Range request, now with a sane budget —
	// must still be able to scan the series the first call never got to
	// (seriesScanned tracking a budget-cut call as "done" would wrongly
	// skip it forever) and find the id.
	title, found, _ := c.fetchSeriesEpisodeTitle("900")
	if !found {
		t.Fatal("expected the id to resolve once the crawl actually gets to scan the series")
	}
	if want := "Show One S01E01 — Target Episode"; title != want {
		t.Fatalf("title = %q, want %q", title, want)
	}
}
