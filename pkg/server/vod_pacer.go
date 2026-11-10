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
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// vodCacheClient is used exclusively by fetchToFile. No global timeout so large files
// can be downloaded fully; transport-level timeouts prevent infinite stalls.
//
// A download keeps one connection for as long as it can: the header, the
// container index at the end and the rest of the file are requested one after
// the other on the same keep-alive connection (see fetchToFile). New
// connections go through vodConnPacer.
var vodCacheClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           vodConnPacer.DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	},
}

// vodConnPacer spaces new provider connections by at least a second after the
// last one closed. Providers allowing a single connection per account track
// connections lazily: one just closed still counts for a moment, and the limit
// is enforced by cutting the oldest connection still open, so reconnecting at
// once can get the new connection cut a few seconds later. Reusing a
// keep-alive connection does not dial, so it is not delayed.
var vodConnPacer = &connPacer{spacing: time.Second}

// connPacer is a dialer that opens a connection only once spacing has passed
// since the last connection it dialed was closed.
type connPacer struct {
	mu        sync.Mutex
	spacing   time.Duration
	lastClose time.Time
	dialer    net.Dialer
}

func (p *connPacer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	for {
		p.mu.Lock()
		wait := time.Until(p.lastClose.Add(p.spacing))
		p.mu.Unlock()
		if wait <= 0 {
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
	c, err := p.dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &pacedConn{Conn: c, p: p}, nil
}

// pacedConn records when it is closed in its pacer.
type pacedConn struct {
	net.Conn
	p    *connPacer
	once sync.Once
}

func (c *pacedConn) Close() error {
	c.once.Do(func() {
		c.p.mu.Lock()
		c.p.lastClose = time.Now()
		c.p.mu.Unlock()
	})
	return c.Conn.Close()
}
