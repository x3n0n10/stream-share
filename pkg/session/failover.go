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
	"github.com/lucasduport/stream-share/pkg/utils"
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
	// A takeover waits for the slot of the stream its viewer just left.
	wait := buffer.takeover || (class < provider.Foreground && sm.providerSlots.WouldGrant(provider.Foreground))
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

// dialOwn dials the own provider within budget (0: no limit). A connection
// limit answer gets one more try after limitRetryWait, still within budget: a
// panel can keep counting the connection just closed (a channel switch, a VOD
// that stopped) for a moment, and a fallback should not get the channel for it.
func (sm *SessionManager) dialOwn(ctx context.Context, budget time.Duration, u *url.URL) (*http.Response, *UpstreamError) {
	start := time.Now()
	resp, uerr := sm.dialWithin(ctx, budget, u, nil)
	if uerr == nil || (uerr.StatusCode != 456 && uerr.StatusCode != 461) {
		return resp, uerr
	}
	var left time.Duration
	if budget > 0 {
		if left = budget - time.Since(start) - sm.limitRetryWait; left <= 0 {
			return nil, uerr
		}
	}
	t := time.NewTimer(sm.limitRetryWait)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		return nil, uerr
	}
	return sm.dialWithin(ctx, left, u, nil)
}

// FailoverTarget is a fallback instance asked to serve a live channel this
// instance cannot start on its own provider.
type FailoverTarget struct {
	Name   string // display name, the only way logs refer to it
	URL    *url.URL
	Header http.Header
}

// SetFailoverTargets sets the fallback instances for a live stream, in order;
// fn gets the stream key and must do no I/O. nil disables failover.
func (sm *SessionManager) SetFailoverTargets(fn func(streamID string) []FailoverTarget) {
	sm.failoverTargets = fn
}

// failoverReason describes a failed dial for logs without uerr.Err, which
// names the address dialled: "<code> <meaning>" or the meaning alone.
func (sm *SessionManager) failoverReason(uerr *UpstreamError) string {
	code, meaning, _ := sm.errorCatalog.Lookup(uerr)
	if r := strings.TrimSpace(code + " " + strings.ToLower(meaning)); r != "" {
		return r
	}
	return strings.ToLower(uerr.Key)
}

// failoverAttempt is one start of a failover stream: the own provider when it
// grants a slot politely and answers within ownTimeout (0: no limit), else
// each fallback in order. It returns the own lease when the own provider
// serves. When all fail, the error is the own provider's: the slate shows it.
func (sm *SessionManager) failoverAttempt(ctx context.Context, buffer *StreamBuffer, upstreamURL *url.URL, ownTimeout time.Duration) (*http.Response, *provider.Lease, *UpstreamError) {
	lease, ownErr := sm.acquirePolite(ctx, buffer)
	if ownErr == nil {
		buffer.ownSlot.Store(true)
		// Watch from the grant: a preemption during the dial (unbounded on a
		// slate retry) stops the stream, which cancels the dial.
		stopWatch := sm.watchPreemption(ctx, buffer, lease)
		resp, uerr := sm.dialOwn(ctx, ownTimeout, upstreamURL)
		if uerr == nil {
			return resp, lease, nil
		}
		// A failed or slating stream holds no slot.
		stopWatch()
		buffer.ownSlot.Store(false)
		lease.Release()
		ownErr = uerr
	}
	label := sm.streamLabel(buffer.streamID)
	for _, target := range sm.failoverTargets(buffer.streamID) {
		if sm.stopRequested(ctx, buffer) {
			break
		}
		resp, err := sm.dialWithin(ctx, sm.failoverPeerTimeout, target.URL, target.Header)
		if err != nil {
			utils.WarnLog("Failover: %s not served by %s (%s)", label, target.Name, sm.failoverReason(err))
			continue
		}
		sm.streamLock.RLock()
		ss := sm.streamSessions[buffer.streamID]
		sm.streamLock.RUnlock()
		if ss != nil {
			ss.SetVia(target.Name)
			sm.recordHistoryVia(buffer.streamID, target.Name)
		}
		utils.InfoLog("Failover: %s served via %s (own provider: %s)", label, target.Name, sm.failoverReason(ownErr))
		return resp, nil, nil
	}
	return nil, nil, ownErr
}
