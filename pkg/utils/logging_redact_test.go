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

package utils

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestLogRedactsRegisteredSecrets(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevDebug := log.Writer(), Config.DebugLoggingEnabled
	log.SetOutput(&buf)
	Config.DebugLoggingEnabled = true
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		Config.DebugLoggingEnabled = prevDebug
		redactor.Store(nil)
		secretList = nil
	})
	RegisterSecrets("provpass1", "user name", "abc")
	DebugLog("up: http://h/prefix/user%%20name/provpass1/42.ts?password=provpass1 abc")
	InfoLog("again %s", "user+name provpass1")
	out := buf.String()
	for _, s := range []string{"provpass1", "user name", "user%20name", "user+name"} {
		if strings.Contains(out, s) {
			t.Fatalf("log leaks %q: %q", s, out)
		}
	}
	if !strings.Contains(out, " abc") {
		t.Fatalf("3-char secret must not be registered: %q", out)
	}
}

func TestRedactionPrefersLongestSecret(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevDebug := log.Writer(), Config.DebugLoggingEnabled
	log.SetOutput(&buf)
	Config.DebugLoggingEnabled = true
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		Config.DebugLoggingEnabled = prevDebug
		redactor.Store(nil)
		secretList = nil
	})
	RegisterSecrets("mark", "mark2024")
	DebugLog("/live/mark/mark2024/1")
	if out := buf.String(); strings.Contains(out, "mark2024") || strings.Contains(out, "2024") {
		t.Fatalf("log leaks part of the longer secret: %q", out)
	}
}
