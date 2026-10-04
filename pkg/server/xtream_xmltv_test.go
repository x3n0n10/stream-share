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
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

// A client authenticating with the custom proxy credentials must never have
// them forwarded to the provider: xmltv.php has to swap in the Xtream ones.
func TestXMLTVUsesProviderCredentialsUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotUser, gotPass string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.URL.Query().Get("username")
		gotPass = r.URL.Query().Get("password")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<tv><channel id="a"><display-name>A</display-name></channel></tv>`))
	}))
	defer upstream.Close()

	c := &Config{ProxyConfig: &config.ProxyConfig{
		User:           "custom-login",
		Password:       "custom-pass",
		XtreamBaseURL:  upstream.URL,
		XtreamUser:     "provider-user",
		XtreamPassword: "provider-pass",
	}}

	r := gin.New()
	r.GET("/xmltv.php", c.authenticate, c.xtreamXMLTV)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/xmltv.php?username=custom-login&password=custom-pass", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if gotUser != "provider-user" || gotPass != "provider-pass" {
		t.Errorf("upstream saw %q/%q, want provider-user/provider-pass", gotUser, gotPass)
	}
}
