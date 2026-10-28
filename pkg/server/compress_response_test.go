package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// A per-request rewritten body is gzipped only for clients that accept it.
func TestServeBytesCompressed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	serve := func(acceptEncoding string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/get.php", nil)
		if acceptEncoding != "" {
			ctx.Request.Header.Set("Accept-Encoding", acceptEncoding)
		}
		serveBytesCompressed(ctx, "application/octet-stream", []byte("#EXTM3U\n"))
		return w
	}

	plain := serve("")
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != "#EXTM3U\n" {
		t.Fatalf("plain: enc=%q body=%q", plain.Header().Get("Content-Encoding"), plain.Body.String())
	}

	gz := serve("gzip")
	if gz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip: Content-Encoding = %q", gz.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != "#EXTM3U\n" {
		t.Fatalf("gzip body = %q", got)
	}
}
