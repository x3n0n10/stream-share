package xtream

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The client-level Timeout also bounds body reads; GetXMLTV must not be
// subject to it, or large EPG downloads fail with "context deadline exceeded".
func TestGetXMLTVIgnoresClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12")
		_, _ = w.Write([]byte("<tv>"))
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("</tv>..."))
	}))
	defer srv.Close()

	c, err := New("u", "p", srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Client.Timeout = 100 * time.Millisecond

	body, err := c.GetXMLTV()
	if err != nil {
		t.Fatalf("GetXMLTV: %v", err)
	}
	if string(body) != "<tv></tv>..." {
		t.Errorf("body = %q", body)
	}
}
