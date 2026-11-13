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

package session

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/provider"
)

// FailoverIdentityPrefix starts the internal username of a viewer another
// instance forwards to this one ("failover:<instance>/<n>"). Such a stream is
// lent: a guest that never gets a slate and yields to the household.
const FailoverIdentityPrefix = "failover:"

// IsFailoverIdentity reports whether username is a forwarded viewer's identity.
func IsFailoverIdentity(username string) bool {
	return strings.HasPrefix(username, FailoverIdentityPrefix)
}

// Slating reports whether streamID is currently showing its error slate
// instead of provider video.
func (sm *SessionManager) Slating(streamID string) bool {
	sm.streamLock.RLock()
	buffer, ok := sm.streamBuffers[streamID]
	sm.streamLock.RUnlock()
	return ok && buffer.slating.Load()
}

// cancelBody releases the dial's context once the body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// dialWithin is dial with a limit of d on getting the response headers only:
// once they arrive the body stays readable until ctx ends (or it is closed).
func (sm *SessionManager) dialWithin(ctx context.Context, d time.Duration, u *url.URL, header http.Header) (*http.Response, *UpstreamError) {
	if d <= 0 {
		return sm.dial(ctx, u, header)
	}
	dctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(d, cancel)
	resp, uerr := sm.dial(dctx, u, header)
	if !timer.Stop() {
		if resp != nil {
			_ = resp.Body.Close()
		}
		cancel()
		return nil, &UpstreamError{Key: KeyTimeout}
	}
	if uerr != nil {
		cancel()
		return nil, uerr
	}
	resp.Body = cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// acquirePolite gets a provider slot for a lent stream without ever taking one
// from a stream of the same class: it is refused at once when the household
// holds the slot, and waits at most slotWait for a guest that is leaving.
func (sm *SessionManager) acquirePolite(ctx context.Context, buffer *StreamBuffer) (*provider.Lease, *UpstreamError) {
	class := buffer.audienceClass()
	// A guest-class request also waits when a guest-class or background holder
	// has the slot: that is usually the stream this caller just dropped.
	wait := class < provider.Foreground && sm.providerSlots.WouldGrant(provider.Foreground)
	if !wait && !sm.providerSlots.WouldGrant(class) {
		return nil, &UpstreamError{Key: KeyAllInUse}
	}
	actx, cancel := context.WithTimeout(ctx, sm.slotWait)
	defer cancel()
	lease, err := sm.providerSlots.AcquireClass(actx, "live "+sm.streamLabel(buffer.streamID),
		func() provider.Class { return buffer.audienceClass() }, provider.Resume)
	if err != nil {
		return nil, &UpstreamError{Key: KeyAllInUse}
	}
	return lease, nil
}
