/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
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
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

// diskCatalog is a provider catalogue (a JSON array) kept in the file raw, so
// a list of hundreds of MB costs no heap. The JSON players get is rendered
// once per client-facing base URL to raw+".render-<hash>" (plus its .gz
// sibling); a category slice is cut from that file by the byte spans recorded
// while rendering, so no list is ever held in memory whole.
type diskCatalog struct {
	raw     string
	mu      sync.Mutex
	renders []*diskRender // oldest first, at most maxCatalogRenders

	// renderMu runs one render at a time per catalogue; a base already
	// rendered is looked up under mu alone, so it never waits for one.
	renderMu  sync.Mutex
	harvested bool // guarded by renderMu: a render has fed the channel index and upstream catchup
}

// diskRender is a whole list rendered for base: its file, and per category id
// the spans of that category's entries in it, in list order.
type diskRender struct {
	base  string
	path  string
	spans map[string][]catalogSpan
}

type catalogSpan struct {
	off int64
	n   int32
}

// catalogFileGrace is how long a replaced catalogue's files outlive its entry,
// so a request still reading them can finish.
var catalogFileGrace = 10 * time.Minute

func newDiskCatalog(raw string) *diskCatalog { return &diskCatalog{raw: raw} }

// walkCatalogFile decodes the elements of the JSON array in path one at a
// time until fn returns false. Bytes after the array are ignored, as the
// provider client ignores them.
func walkCatalogFile(path string, fn func(v interface{}) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil {
		return err
	} else if tok != json.Delim('[') {
		return errors.New("catalogue file is not a JSON array")
	}
	for dec.More() {
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			return err
		}
		if !fn(v) {
			return nil
		}
	}
	// The closing ']': a truncated file is an error, not a shorter list.
	_, err = dec.Token()
	return err
}

func (d *diskCatalog) each(fn func(m map[string]interface{}) bool) error {
	return walkCatalogFile(d.raw, func(v interface{}) bool {
		m, ok := v.(map[string]interface{})
		return !ok || fn(m)
	})
}

// ensureRender returns the list rendered for base, rendering it on first use
// and then dropping the oldest render beyond maxCatalogRenders. A failed
// render leaves no files and is not stored, so the next request retries.
func (d *diskCatalog) ensureRender(c *Config, action, base string) (*diskRender, error) {
	if r := d.renderOf(base); r != nil {
		return r, nil
	}
	d.renderMu.Lock()
	defer d.renderMu.Unlock()
	if r := d.renderOf(base); r != nil {
		return r, nil // rendered while this request waited
	}
	// The file name carries a hash, never the base: the Host header is client input.
	sum := sha256.Sum256([]byte(base))
	path := fmt.Sprintf("%s.render-%x", d.raw, sum[:4])
	// A base whose hash collides would share the file this render overwrites.
	d.mu.Lock()
	d.renders = slices.DeleteFunc(d.renders, func(r *diskRender) bool { return r.path == path })
	d.mu.Unlock()
	r := &diskRender{base: base, path: path, spans: map[string][]catalogSpan{}}
	if err := d.writeRender(c, action, r, !d.harvested); err != nil {
		for _, p := range []string{r.path + ".part", r.path, r.path + ".gz"} {
			_ = os.Remove(p)
		}
		return nil, err
	}
	d.harvested = true
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.renders) == maxCatalogRenders {
		// A request still streaming the old file keeps reading its open descriptor.
		_ = os.Remove(d.renders[0].path)
		_ = os.Remove(d.renders[0].path + ".gz")
		d.renders = slices.Delete(d.renders, 0, 1) // zeroes the freed slot, so its span index can be collected
	}
	d.renders = append(d.renders, r)
	return r, nil
}

// renderOf returns the stored render for base, or nil.
func (d *diskCatalog) renderOf(base string) *diskRender {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.renders {
		if r.base == base {
			return r
		}
	}
	return nil
}

// writeRender writes to r.path what renderCatalog(action, list, harvest,
// r.base) would encode, one item at a time: json.Marshal of a list is its
// items' encodings joined by commas. Non-object items pass through
// unfiltered, as in filterEntries, and belong to no category.
func (d *diskCatalog) writeRender(c *Config, action string, r *diskRender, harvest bool) error {
	a, known := catalogActions[action]
	filtering := known && c.catalogFilter.ActiveFor(a.kind)
	var names map[string]string
	if filtering {
		names = c.categoryNames(a.pair)
	}
	live := action == "get_live_streams"
	harvesting := live && harvest
	flag := live && c.catchupManager != nil && c.catchupManager.IsEnabled()
	channels, upstream := newChannelHarvest(), map[string]bool{}

	tmp := r.path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	var off int64
	// bufio keeps the first write error and returns it from Flush.
	put := func(b []byte) { _, _ = bw.Write(b); off += int64(len(b)) }
	put([]byte{'['})
	var merr error
	err = walkCatalogFile(d.raw, func(v interface{}) bool {
		m, isObj := v.(map[string]interface{})
		if isObj {
			if harvesting {
				// Names come from the full list, as in renderCatalog.
				channels.add(m)
			}
			if filtering && !c.keepCatalogEntry(a.kind, m, names) {
				return true
			}
			if flag {
				v = c.catchupFlagged(m, upstream)
			}
		}
		var b []byte
		if b, merr = json.Marshal(c.clientSafeValue(r.base, v)); merr != nil {
			return false
		}
		if off > 1 { // an item already follows the '['
			put([]byte{','})
		}
		start := off
		put(b)
		if isObj {
			sp := catalogSpan{start, int32(len(b))}
			add := func(id string) {
				s := r.spans[id]
				if len(s) > 0 && s[len(s)-1].off == start {
					return // the entry lists this category twice
				}
				r.spans[id] = append(s, sp)
			}
			// Every id inCategory matches.
			add(catalogString(m["category_id"]))
			ids, _ := m["category_ids"].([]interface{})
			for _, id := range ids {
				add(catalogString(id))
			}
		}
		return true
	})
	put([]byte{']'})
	if err == nil {
		err = merr
	}
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, r.path)
	}
	if err == nil {
		err = writeGzipSibling(r.path)
	}
	if err != nil {
		return err
	}
	if harvesting {
		c.applyChannelHarvest(channels)
	}
	if flag && harvest {
		c.catchupManager.SetUpstreamCatchup(upstream)
	}
	return nil
}

// serveWhole sends the rendered file. Unlike sendCatalogJSON it makes no
// debug-mode dump: the body is already a file in the cache folder.
func (d *diskCatalog) serveWhole(ctx *gin.Context, c *Config, action string) error {
	r, err := d.ensureRender(c, action, c.requestBaseURL(ctx))
	if err != nil {
		return err
	}
	serveFileCompressed(ctx, r.path, "application/json; charset=utf-8")
	return nil
}

// serveSlice sends the category's entries, read from the rendered file by
// their spans. An unknown category is [].
func (d *diskCatalog) serveSlice(ctx *gin.Context, c *Config, action, categoryID string) error {
	r, err := d.ensureRender(c, action, c.requestBaseURL(ctx))
	if err != nil {
		return err
	}
	f, err := os.Open(r.path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := responseWriter(ctx, "application/json; charset=utf-8")
	_, _ = w.Write([]byte{'['})
	var buf []byte
	for i, s := range r.spans[categoryID] {
		if cap(buf) < int(s.n) {
			buf = make([]byte, s.n)
		}
		buf = buf[:s.n]
		if _, err := f.ReadAt(buf, s.off); err != nil {
			return err // left unclosed, so a gzip client sees a truncated stream
		}
		if i > 0 {
			_, _ = w.Write([]byte{','})
		}
		_, _ = w.Write(buf)
	}
	_, _ = w.Write([]byte{']'})
	return w.Close()
}

// removeCatalogFiles deletes a disk catalogue's files, given its raw path:
// the raw list and every base URL's render.
func removeCatalogFiles(raw string) {
	renders, _ := filepath.Glob(raw + ".render-*")
	for _, p := range append(renders, raw) {
		_ = os.Remove(p)
	}
}

// fetchCatalogToDisk is catalogFetcher in disk mode for a whole stream list:
// the body goes straight to a file and stays there when it is a non-empty
// array; any other answer is decoded and handled as in memory mode. Failures
// leave no file and come back as *catalogResultError, like a provider failure.
func (c *Config) fetchCatalogToDisk(client *xtreamapi.Client, action string, q url.Values) (interface{}, error) {
	key := c.catalogKey(action)
	dir, err := catalogRunDir()
	if err != nil {
		return nil, &catalogResultError{err: fmt.Errorf("%s: catalogue folder: %w", action, err)}
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.json", catalogKeyLabel(key), time.Now().UnixNano()))
	httpcode, err := client.ActionToFile(context.Background(), c.ProxyConfig, action, q, path)
	if err != nil {
		return nil, &catalogResultError{code: httpcode,
			err: fmt.Errorf("%s failed (HTTP %d): %w", action, httpcode, err)}
	}
	if httpcode != http.StatusOK {
		// An empty body: no file, and Action would have answered the fallback.
		return nil, &catalogResultError{data: xtreamapi.FallbackForAction(action), code: httpcode}
	}
	kind, small, err := xtreamapi.ClassifyCatalogFile(path, action)
	var data interface{}
	switch {
	case err != nil:
		_ = os.Remove(path)
		return nil, &catalogResultError{data: small, code: httpcode,
			err: fmt.Errorf("%s failed (HTTP %d): %w", action, httpcode, err)}
	case kind == xtreamapi.CatalogFileArray:
		data = newDiskCatalog(path)
	default:
		_ = os.Remove(path)
		if !usableCatalog(small) {
			return nil, &catalogResultError{data: small, code: httpcode}
		}
		data = &memoryCatalog{data: small}
	}
	// This answer replaces the cached entry: drop its files once readers are done.
	if e := peekCatalogEntry(key); e != nil {
		if old, ok := e.data.(*diskCatalog); ok {
			// shortcut: old files are kept for a fixed 10 min grace, not refcounted; switch to refcounting if a reader can hold an entry longer
			raw := old.raw // only the path: holding old would keep its span index in memory for the whole grace
			time.AfterFunc(catalogFileGrace, func() { removeCatalogFiles(raw) })
		}
	}
	return data, nil
}
