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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// deadlineWriter is a ResponseWriter honoring write deadlines: with block
// set, a write waits for the deadline and fails as a timed-out socket does;
// with err set, writes fail at once with it.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	block    bool
	err      error
}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error { w.deadline = t; return nil }

func (w *deadlineWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.block {
		time.Sleep(time.Until(w.deadline))
		return 0, os.ErrDeadlineExceeded
	}
	return w.ResponseRecorder.Write(p)
}

func stallWriterFor(w http.ResponseWriter, timeout time.Duration) *stallWriter {
	ctx, _ := gin.CreateTestContext(w)
	return newStallWriter(ctx.Writer, timeout)
}

func TestStallWriter(t *testing.T) {
	t.Run("blocked write times out as a stall", func(t *testing.T) {
		sw := stallWriterFor(&deadlineWriter{ResponseRecorder: httptest.NewRecorder(), block: true}, 100*time.Millisecond)
		start := time.Now()
		if _, err := sw.Write([]byte("x")); err == nil {
			t.Fatal("write should fail")
		}
		if !sw.stalled {
			t.Fatal("a write that timed out is a stall")
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("write took %s, want about the timeout", d)
		}
	})
	t.Run("client gone is not a stall", func(t *testing.T) {
		sw := stallWriterFor(&deadlineWriter{ResponseRecorder: httptest.NewRecorder(), err: syscall.EPIPE}, time.Second)
		if _, err := sw.Write([]byte("x")); !errors.Is(err, syscall.EPIPE) || sw.stalled {
			t.Fatalf("err = %v, stalled = %v; want EPIPE, not a stall", err, sw.stalled)
		}
	})
	t.Run("real client that stops reading", func(t *testing.T) {
		stalled := make(chan bool, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := stallWriterFor(w, 200*time.Millisecond)
			chunk := make([]byte, 64<<10)
			for i := 0; i < 4096; i++ { // 256 MiB at most, far past socket buffers
				if _, err := sw.Write(chunk); err != nil {
					break
				}
			}
			stalled <- sw.stalled
		}))
		defer srv.Close()
		resp, err := http.Get(srv.URL) // headers only; the body is never read
		if err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-stalled:
			if !got {
				t.Fatal("a client that stopped reading should count as a stall")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the write never timed out")
		}
		_ = resp.Body.Close()
	})
	t.Run("normal write", func(t *testing.T) {
		rec := httptest.NewRecorder()
		sw := stallWriterFor(&deadlineWriter{ResponseRecorder: rec}, time.Second)
		if _, err := sw.Write([]byte("x")); err != nil || sw.stalled || rec.Body.String() != "x" {
			t.Fatalf("err = %v, stalled = %v, body %q", err, sw.stalled, rec.Body.String())
		}
	})
}
