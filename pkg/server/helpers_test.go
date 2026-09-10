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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func writeServeLocalFileRangeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video.ts")
	if err := os.WriteFile(path, []byte("0123456789"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestServeLocalFileRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	filePath := writeServeLocalFileRangeFixture(t)

	call := func(method, rangeHeader string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(method, "/video.ts", nil)
		if rangeHeader != "" {
			ctx.Request.Header.Set("Range", rangeHeader)
		}
		serveLocalFileRange(ctx, filePath, "video/mp2t", "", false)
		return w
	}

	t.Run("full GET returns the whole file", func(t *testing.T) {
		w := call(http.MethodGet, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		body, _ := io.ReadAll(w.Body)
		if string(body) != "0123456789" {
			t.Errorf("body = %q, want %q", string(body), "0123456789")
		}
		if got := w.Header().Get("Content-Length"); got != "10" {
			t.Errorf("Content-Length = %q, want %q", got, "10")
		}
		if got := w.Header().Get("Content-Type"); got != "video/mp2t" {
			t.Errorf("Content-Type = %q, want %q", got, "video/mp2t")
		}
		if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("Accept-Ranges = %q, want %q (set by http.ServeContent)", got, "bytes")
		}
	})

	t.Run("Range GET returns 206 with only the requested bytes", func(t *testing.T) {
		w := call(http.MethodGet, "bytes=2-4")
		if w.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", w.Code)
		}
		body, _ := io.ReadAll(w.Body)
		if string(body) != "234" {
			t.Errorf("body = %q, want %q", string(body), "234")
		}
		if got := w.Header().Get("Content-Range"); got != "bytes 2-4/10" {
			t.Errorf("Content-Range = %q, want %q", got, "bytes 2-4/10")
		}
	})

	t.Run("suffix range returns the last N bytes", func(t *testing.T) {
		w := call(http.MethodGet, "bytes=-3")
		if w.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", w.Code)
		}
		body, _ := io.ReadAll(w.Body)
		if string(body) != "789" {
			t.Errorf("body = %q, want %q", string(body), "789")
		}
	})

	t.Run("out-of-range request returns 416", func(t *testing.T) {
		w := call(http.MethodGet, "bytes=100-200")
		if w.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status = %d, want 416", w.Code)
		}
	})

	t.Run("HEAD returns headers with no body", func(t *testing.T) {
		w := call(http.MethodHead, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		body, _ := io.ReadAll(w.Body)
		if len(body) != 0 {
			t.Errorf("HEAD body = %q, want empty", string(body))
		}
		if got := w.Header().Get("Content-Length"); got != "10" {
			t.Errorf("Content-Length = %q, want %q", got, "10")
		}
	})

	t.Run("asAttachment sets Content-Disposition", func(t *testing.T) {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/download", nil)
		serveLocalFileRange(ctx, filePath, "video/mp2t", "My Movie.ts", true)

		want := `attachment; filename="My Movie.ts"`
		if got := w.Header().Get("Content-Disposition"); got != want {
			t.Errorf("Content-Disposition = %q, want %q", got, want)
		}
	})

	t.Run("missing file returns 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/missing", nil)
		serveLocalFileRange(ctx, filepath.Join(t.TempDir(), "nope.ts"), "video/mp2t", "", false)

		// ctx.Writer.Status(), not the raw recorder's w.Code: this early-return
		// branch calls ctx.Status() with no body write, which gin only flushes
		// to the underlying ResponseWriter at the end of the full router's
		// request handling -- calling the handler directly, as this test does,
		// skips that flush. ctx.Writer.Status() reflects the set status either
		// way and is what a real client (going through the real router) sees.
		if got := ctx.Writer.Status(); got != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", got)
		}
	})
}
