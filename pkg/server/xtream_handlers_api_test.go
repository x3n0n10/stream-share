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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func TestXtreamGetRewritesHostPerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	cachedPath := filepath.Join(dir, "cached.m3u")
	cachedBody := "#EXTM3U\n" +
		`#EXTINF:-1 tvg-logo="/img?url=http%3A%2F%2Fupstream.example.com%2Flogo.png", Channel One` + "\n" +
		"/anti/user/pass/0/stream1\n"
	if err := os.WriteFile(cachedPath, []byte(cachedBody), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c := &Config{ProxyConfig: &config.ProxyConfig{
		M3UFileName:        "playlist.m3u",
		M3UCacheExpiration: 24,
		XtreamBaseURL:      "http://upstream.example.com",
		XtreamUser:         "xu",
		XtreamPassword:     "xp",
	}}

	upstreamQ := url.Values{}
	upstreamQ.Set("username", c.XtreamUser.String())
	upstreamQ.Set("password", c.XtreamPassword.String())
	m3uURL, err := url.Parse(c.XtreamBaseURL + "/get.php?" + upstreamQ.Encode())
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	xtreamM3uCacheLock.Lock()
	xtreamM3uCache[m3uURL.String()] = cacheMeta{cachedPath, time.Now()}
	xtreamM3uCacheLock.Unlock()

	call := func(host string) string {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/get.php", nil)
		ctx.Request.Host = host
		c.xtreamGet(ctx)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	bodyA := call("host-a.example.com")
	bodyB := call("host-b.example.com:9090")

	if !strings.Contains(bodyA, "http://host-a.example.com/anti/user/pass/0/stream1") {
		t.Errorf("bodyA %q does not use host-a", bodyA)
	}
	if !strings.Contains(bodyB, "http://host-b.example.com:9090/anti/user/pass/0/stream1") {
		t.Errorf("bodyB %q does not use host-b", bodyB)
	}
	if strings.Contains(bodyA, "host-b") || strings.Contains(bodyB, "host-a") {
		t.Errorf("the same cached file leaked the other request's host: bodyA=%q bodyB=%q", bodyA, bodyB)
	}
}

func TestXtreamPlayerAPILoginUsesRequestHost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c := &Config{ProxyConfig: &config.ProxyConfig{HostConfig: &config.HostConfiguration{Port: 8080}}}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
	ctx.Request.Host = "viewer.example.com:9090"

	c.xtreamPlayerAPI(ctx, url.Values{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ServerInfo struct {
			URL  string `json:"url"`
			Port string `json:"port"`
		} `json:"server_info"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.ServerInfo.URL != "http://viewer.example.com" {
		t.Errorf("server_info.url = %q, want %q", resp.ServerInfo.URL, "http://viewer.example.com")
	}
	if resp.ServerInfo.Port != "9090" {
		t.Errorf("server_info.port = %q, want %q", resp.ServerInfo.Port, "9090")
	}
}

func TestXtreamPlayerAPILoginFallsBackToListenPortWhenHostHasNone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := &Config{ProxyConfig: &config.ProxyConfig{HostConfig: &config.HostConfiguration{Port: 8080}}}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
	ctx.Request.Host = "viewer.example.com"

	c.xtreamPlayerAPI(ctx, url.Values{})

	var resp struct {
		ServerInfo struct {
			Port string `json:"port"`
		} `json:"server_info"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.ServerInfo.Port != "8080" {
		t.Errorf("server_info.port = %q, want fallback listen port %q", resp.ServerInfo.Port, "8080")
	}
}
