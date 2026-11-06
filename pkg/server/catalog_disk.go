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
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/gin-gonic/gin"
)

// diskCatalog is a provider catalogue (a JSON array) kept in the file raw, so
// a list of hundreds of MB costs no heap. The JSON players get is rendered
// once to raw+".render" (plus its .gz sibling); a category slice is cut from
// that file by the byte spans recorded while rendering, so no list is ever
// held in memory whole.
type diskCatalog struct {
	raw    string
	mu     sync.Mutex
	render *diskRender
}

// diskRender is a rendered whole list: its file, and per category id the
// spans of that category's entries in it, in list order.
type diskRender struct {
	path  string
	spans map[string][]catalogSpan
}

type catalogSpan struct {
	off int64
	n   int32
}

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

// ensureRender returns the rendered list, rendering it on first use. A failed
// render leaves no files and is not stored, so the next request retries.
func (d *diskCatalog) ensureRender(c *Config, action string) (*diskRender, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.render != nil {
		return d.render, nil
	}
	r := &diskRender{path: d.raw + ".render", spans: map[string][]catalogSpan{}}
	if err := d.writeRender(c, action, r); err != nil {
		for _, p := range []string{r.path + ".part", r.path, r.path + ".gz"} {
			_ = os.Remove(p)
		}
		return nil, err
	}
	d.render = r
	return r, nil
}

// writeRender writes to r.path what renderCatalog(action, list, true) would
// encode, one item at a time: json.Marshal of a list is its items' encodings
// joined by commas. Non-object items pass through unfiltered, as in
// filterEntries, and belong to no category.
func (d *diskCatalog) writeRender(c *Config, action string, r *diskRender) error {
	a, known := catalogActions[action]
	filtering := known && c.catalogFilter.ActiveFor(a.kind)
	var names map[string]string
	if filtering {
		names = c.categoryNames(a.pair)
	}
	live := action == "get_live_streams"
	flag := live && c.catchupManager != nil && c.catchupManager.IsEnabled()
	harvest, upstream := newChannelHarvest(), map[string]bool{}

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
			if live {
				// Names come from the full list, as in renderCatalog.
				harvest.add(m)
			}
			if filtering && !c.keepCatalogEntry(a.kind, m, names) {
				return true
			}
			if flag {
				v = c.catchupFlagged(m, upstream)
			}
		}
		var b []byte
		if b, merr = json.Marshal(v); merr != nil {
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
	if live {
		c.applyChannelHarvest(harvest)
	}
	if flag {
		c.catchupManager.SetUpstreamCatchup(upstream)
	}
	return nil
}

// serveWhole sends the rendered file. Unlike sendCatalogJSON it makes no
// debug-mode dump: the body is already a file in the cache folder.
func (d *diskCatalog) serveWhole(ctx *gin.Context, c *Config, action string) error {
	r, err := d.ensureRender(c, action)
	if err != nil {
		return err
	}
	serveFileCompressed(ctx, r.path, "application/json; charset=utf-8")
	return nil
}

// serveSlice sends the category's entries, read from the rendered file by
// their spans. An unknown category is [].
func (d *diskCatalog) serveSlice(ctx *gin.Context, c *Config, action, categoryID string) error {
	r, err := d.ensureRender(c, action)
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

// remove deletes the catalogue's files.
func (d *diskCatalog) remove() {
	for _, p := range []string{d.raw, d.raw + ".render", d.raw + ".render.gz"} {
		_ = os.Remove(p)
	}
}
