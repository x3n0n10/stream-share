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

package xtream

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/utils"
)

func TestActionDebugLogMasksProviderPassword(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevDebug := log.Writer(), utils.Config.DebugLoggingEnabled
	log.SetOutput(&buf)
	utils.Config.DebugLoggingEnabled = true
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		utils.Config.DebugLoggingEnabled = prevDebug
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	client, err := New("provuser", "provpass", srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.Action(&config.ProxyConfig{}, "get_vod_streams", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Xtream raw request") {
		t.Fatalf("debug line missing: %q", buf.String())
	}
	if strings.Contains(buf.String(), "provpass") {
		t.Fatalf("debug log leaks the provider password: %q", buf.String())
	}
}
