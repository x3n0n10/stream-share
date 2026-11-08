/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// captureDebugLog turns debug logging on and returns the buffer the standard
// log package now writes to; both are restored when the test ends.
func captureDebugLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevDebug := log.Writer(), utils.Config.DebugLoggingEnabled
	log.SetOutput(&buf)
	utils.Config.DebugLoggingEnabled = true
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		utils.Config.DebugLoggingEnabled = prevDebug
	})
	return &buf
}

func TestDebugLogsRedactCredentials(t *testing.T) {
	buf := captureDebugLog(t)
	gin.SetMode(gin.TestMode)
	c := &Config{ProxyConfig: &config.ProxyConfig{User: "u1", Password: "s3cret"}}
	r := gin.New()
	r.Use(c.quietRequestLogger())
	r.GET("/player_api.php", c.authenticate, func(ctx *gin.Context) { ctx.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/player_api.php?username=u1&password=s3cret", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(buf.String(), "/player_api.php") {
		t.Fatalf("debug log did not record the request: %q", buf.String())
	}
	if strings.Contains(buf.String(), "s3cret") {
		t.Fatalf("debug log leaks the password: %q", buf.String())
	}
}

func TestRedactRequestURICoversCredentialRoutes(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{
		User: "u1", Password: "s3cret", XtreamUser: "prov", XtreamPassword: "provpass",
	}, endpointAntiColision: "anti"}
	cases := []struct{ path, route string }{
		{"/live/u1/s3cret/12.ts", "/live/:username/:password/:id"},
		{"/live/prov/provpass/12.ts", "/live/prov/provpass/:id"},
		{"/hlsr/tok/prov/provpass/ch/hash/chunk.ts", "/hlsr/:token/prov/provpass/:channel/:hash/:chunk"},
		{"/anti/prov/provpass/3/file.ts", "/anti/prov/provpass/:track/*file"},
		{"/share/abcdef/live.ts", "/share/:token/live.ts"},
		{"/download/abcdef", "/download/:token"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := c.redactRequestURI(&url.URL{Path: tc.path}, tc.route)
			for _, s := range []string{"s3cret", "provpass", "abcdef"} {
				if strings.Contains(got, s) {
					t.Fatalf("redacted %q still contains %q", got, s)
				}
			}
		})
	}
}

func TestStreamDebugLogMasksProviderPassword(t *testing.T) {
	buf := captureDebugLog(t)
	utils.RegisterSecrets("provpass1")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer up.Close()
	c := &Config{ProxyConfig: &config.ProxyConfig{XtreamUser: "provuser", XtreamPassword: "provpass1"}}
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/live/provuser/provpass1/42.ts", nil)
	u, _ := url.Parse(up.URL + "/prefix/provuser/provpass1/42.ts")
	c.stream(ctx, u)
	if !strings.Contains(buf.String(), "Proxying to upstream URL") || strings.Contains(buf.String(), "provpass1") {
		t.Fatalf("log missing line or leaks password: %q", buf.String())
	}
}
