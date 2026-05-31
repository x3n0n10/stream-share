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

package session

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// SessionManager handles user sessions and stream multiplexing.
// Lock ordering (always acquire in this order to avoid deadlock):
//
//	userLock → streamLock
type SessionManager struct {
	userSessions       map[string]*types.UserSession   // username -> session
	streamSessions     map[string]*types.StreamSession // streamID -> session
	streamBuffers      map[string]*StreamBuffer        // streamID -> buffer
	db                 *database.DBManager
	tempLinks          map[string]*types.TemporaryLink // token -> temp link
	userLock           sync.RWMutex
	streamLock         sync.RWMutex
	tempLinkLock       sync.RWMutex
	cleanupInterval    time.Duration
	sessionTimeout     time.Duration
	streamTimeout      time.Duration
	tempLinkTimeout    time.Duration
	vodCacheStaleAge   time.Duration
	clientStallTimeout time.Duration // drop a multiplexed client whose buffer stays full this long
	httpClient         *http.Client
	stopChan           chan struct{} // closed by Stop() to terminate background goroutines
	catchupManager     *catchup.Manager
}

// Stream multiplexing tuning.
//
// The upstream is read once and fanned out to every client attached to the same
// stream. Delivery is back-pressured: the pump does not advance to the next
// upstream chunk until every client has accepted the current one (or been
// dropped). With a single client this reproduces a direct proxy — the upstream
// read rate is gated by how fast that client drains, so TCP back-pressure flows
// all the way to the provider and the stream stays smooth. With multiple clients
// the pump runs at the rate of the slowest healthy client; the per-client buffer
// absorbs jitter, and a client that stalls longer than clientStallTimeout is
// dropped so it cannot freeze the shared connection for everyone else.
const (
	streamChunkSize           = 128 * 1024       // upstream read size
	clientBufferChunks        = 32               // per-client jitter buffer (~4MB)
	defaultClientStallTimeout = 30 * time.Second // default for SessionManager.clientStallTimeout
)

// streamClient is a single viewer attached to a StreamBuffer.
type streamClient struct {
	ch       chan []byte   // buffered video chunks awaiting the HTTP writer
	done     chan struct{} // closed once when the client leaves or is dropped
	doneOnce sync.Once
}

// close signals the client to terminate. Safe to call multiple times and from
// either the HTTP side (client disconnected) or the pump (slow-client drop).
func (c *streamClient) close() {
	c.doneOnce.Do(func() { close(c.done) })
}

// StreamBuffer fans a single upstream connection out to multiple clients.
type StreamBuffer struct {
	streamID    string
	upstreamURL string
	active      bool

	// Attached clients, keyed by username.
	clients     map[string]*streamClient
	clientsLock sync.RWMutex

	// Stop signal for the upstream pump.
	stopChan chan struct{}
	stopOnce sync.Once

	// diskBuffer writes each chunk to disk for local catchup/timeshift playback.
	// nil when catchup is disabled or stream type is not live.
	diskBuffer *catchup.DiskBuffer
}

// NewSessionManager creates a new session manager
func NewSessionManager(db *database.DBManager) *SessionManager {
	manager := &SessionManager{
		userSessions:       make(map[string]*types.UserSession),
		streamSessions:     make(map[string]*types.StreamSession),
		streamBuffers:      make(map[string]*StreamBuffer),
		tempLinks:          make(map[string]*types.TemporaryLink),
		db:                 db,
		cleanupInterval:    24 * time.Hour,
		sessionTimeout:     30 * time.Minute,
		streamTimeout:      2 * time.Minute,
		tempLinkTimeout:    24 * time.Hour,
		vodCacheStaleAge:   24 * time.Hour,
		clientStallTimeout: defaultClientStallTimeout,
		stopChan:           make(chan struct{}),
		httpClient: &http.Client{
			// No global Timeout: long-running streams must not be cut after 60s
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   false, // avoid HTTP/2 flow control stalls with IPTV providers
				DisableCompression:  true,  // avoid gzip on video streams
			},
		},
	}

	// Start cleanup routine (stopped by Stop())
	go manager.cleanupRoutine()

	return manager
}

// Stop terminates all background goroutines started by the session manager.
func (sm *SessionManager) Stop() {
	close(sm.stopChan)
}

// sessionSweepInterval controls how often idle sessions and streams are reaped.
// It is deliberately short so a stalled viewer that halts its stream's pump
// releases the shared upstream connection promptly (within streamTimeout of going
// idle), rather than waiting for the daily cache sweep.
const sessionSweepInterval = 30 * time.Second

// cleanupRoutine reaps idle sessions/streams on a short cadence and expired
// links/VOD cache on the (daily) cleanupInterval. It exits when stopChan closes.
func (sm *SessionManager) cleanupRoutine() {
	sessionTicker := time.NewTicker(sessionSweepInterval)
	defer sessionTicker.Stop()
	cacheTicker := time.NewTicker(sm.cleanupInterval)
	defer cacheTicker.Stop()

	for {
		select {
		case <-sm.stopChan:
			return
		case <-sessionTicker.C:
			sm.cleanupExpiredSessions()
			sm.cleanupUnusedStreams()
		case <-cacheTicker.C:
			if sm.db != nil {
				// Remove temporary links past their expiry date
				if count, err := sm.db.CleanupExpiredLinks(); err != nil {
					utils.ErrorLog("Failed to clean expired links: %v", err)
				} else if count > 0 {
					utils.InfoLog("Cleaned %d expired temporary links", count)
				}
				// Remove DB rows whose expires_at has passed
				if _, err := sm.db.CleanupExpiredCache(); err != nil {
					utils.ErrorLog("Failed to clean expired VOD cache entries: %v", err)
				}
				// Delete files (and their DB rows) not accessed within the stale age
				sm.cleanupStaleVODFiles()
			}
		}
	}
}

// cleanupExpiredSessions removes inactive user sessions
func (sm *SessionManager) cleanupExpiredSessions() {
	threshold := time.Now().Add(-sm.sessionTimeout)

	sm.userLock.Lock()
	defer sm.userLock.Unlock()

	for username, session := range sm.userSessions {
		if session.LastActive.Before(threshold) {
			utils.InfoLog("Session expired for user %s (inactive for %s)",
				username, utils.HumanDuration(time.Since(session.LastActive)))

			// If user was watching a stream, remove from viewers
			if session.StreamID != "" {
				sm.streamLock.Lock()
				if streamSession, exists := sm.streamSessions[session.StreamID]; exists {
					if !streamSession.RemoveViewer(username) && streamSession.Active {
						// No more viewers, stop the stream
						sm.stopStream(session.StreamID)
					}
				}
				sm.streamLock.Unlock()
			}

			delete(sm.userSessions, username)
		}
	}
}

// cleanupUnusedStreams stops streams that have no viewers
func (sm *SessionManager) cleanupUnusedStreams() {
	threshold := time.Now().Add(-sm.streamTimeout)

	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()

	for streamID, session := range sm.streamSessions {
		if session.LastRequested.Before(threshold) && session.Active {
			utils.InfoLog("Stream %s has been inactive for %s, stopping",
				streamID, utils.HumanDuration(time.Since(session.LastRequested)))
			sm.stopStream(streamID)
		}
	}
}

// RegisterUser creates or updates a user session
func (sm *SessionManager) RegisterUser(username, ip, userAgent string) *types.UserSession {
	sm.userLock.Lock()
	defer sm.userLock.Unlock()

	now := time.Now()

	// Check if user already has a session
	if session, exists := sm.userSessions[username]; exists {
		session.LastActive = now
		session.IPAddress = ip
		session.UserAgent = userAgent
		return session
	}

	// Create new session
	session := &types.UserSession{
		Username:   username,
		StartTime:  now,
		LastActive: now,
		IPAddress:  ip,
		UserAgent:  userAgent,
	}

	sm.userSessions[username] = session

	// Try to get Discord info if available
	if sm.db != nil {
		discordID, discordName, err := sm.db.GetDiscordByLDAPUser(username)
		if err == nil {
			session.DiscordID = discordID
			session.DiscordName = discordName
			utils.DebugLog("Linked Discord account %s to user %s", discordName, username)
		}
	}

	utils.InfoLog("New session registered for user %s from %s", username, ip)
	return session
}

// GetUserSession retrieves a user session if it exists
func (sm *SessionManager) GetUserSession(username string) *types.UserSession {
	// Use Lock (not RLock) because we mutate LastActive below.
	sm.userLock.Lock()
	defer sm.userLock.Unlock()

	session, exists := sm.userSessions[username]
	if !exists {
		return nil
	}

	session.LastActive = time.Now()
	return session
}

// RequestStream handles a new stream request and implements connection multiplexing
func (sm *SessionManager) RequestStream(username, streamID, streamType, streamTitle string,
	upstreamURL *url.URL) (*StreamBuffer, error) {

	// Get user session, creating if necessary
	var userSession *types.UserSession
	sm.userLock.Lock()
	if session, exists := sm.userSessions[username]; exists {
		userSession = session
	} else {
		userSession = &types.UserSession{
			Username:   username,
			StartTime:  time.Now(),
			LastActive: time.Now(),
		}
		sm.userSessions[username] = userSession
	}

	// Update user session with stream info
	prevStreamID := userSession.StreamID
	userSession.StreamID = streamID
	userSession.StreamType = streamType
	userSession.LastActive = time.Now()
	sm.userLock.Unlock()

	// Handle case where user switches streams
	if prevStreamID != "" && prevStreamID != streamID {
		sm.streamLock.Lock()
		if prevStream, exists := sm.streamSessions[prevStreamID]; exists {
			if !prevStream.RemoveViewer(username) && prevStream.Active {
				// If no more viewers, stop the previous stream
				sm.stopStream(prevStreamID)
			}
		}
		sm.streamLock.Unlock()
	}

	// Check if this stream is already active
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()

	var streamBuffer *StreamBuffer

	// If this stream already exists, attach the user as an additional viewer of the
	// shared upstream connection.
	if existingBuffer, exists := sm.streamBuffers[streamID]; exists && existingBuffer.active {
		utils.InfoLog("User %s joined existing stream %s (multiplexed)", username, streamID)

		if streamSession, exists := sm.streamSessions[streamID]; exists {
			streamSession.AddViewer(username)
			streamSession.LastRequested = time.Now()
		}

		existingBuffer.clientsLock.Lock()
		// If the user already has a client attached (reconnect), drop the old one so
		// its HTTP handler exits cleanly before we install the replacement.
		if old, alreadyClient := existingBuffer.clients[username]; alreadyClient {
			old.close()
			delete(existingBuffer.clients, username)
			utils.InfoLog("User %s reconnected to stream %s; replaced stale client", username, streamID)
		}
		existingBuffer.clients[username] = newStreamClient()
		existingBuffer.clientsLock.Unlock()

		return existingBuffer, nil
	}

	// Create a new stream session
	streamSession := &types.StreamSession{
		StreamID:      streamID,
		StreamType:    streamType,
		StreamTitle:   streamTitle,
		UpstreamURL:   upstreamURL.String(),
		StartTime:     time.Now(),
		LastRequested: time.Now(),
		Viewers:       make(map[string]time.Time),
		Active:        true,
	}
	streamSession.AddViewer(username)
	sm.streamSessions[streamID] = streamSession

	// Create a new stream buffer with the requesting user as the first client
	streamBuffer = &StreamBuffer{
		streamID:    streamID,
		upstreamURL: upstreamURL.String(),
		active:      true,
		clients:     map[string]*streamClient{username: newStreamClient()},
		stopChan:    make(chan struct{}),
	}

	// Start a disk buffer for live streams so catchup/timeshift can rewind.
	if sm.catchupManager != nil && streamType == "live" {
		streamBuffer.diskBuffer = sm.catchupManager.StartBuffer(streamID)
	}

	sm.streamBuffers[streamID] = streamBuffer

	// Start the single upstream pump that fans out to all clients
	go sm.streamToClients(streamBuffer, upstreamURL)

	// Record in database
	if sm.db != nil {
		_, err := sm.db.AddStreamHistory(
			username, streamID, streamType, streamTitle,
			userSession.IPAddress, userSession.UserAgent,
		)
		if err != nil {
			utils.ErrorLog("Failed to record stream history: %v", err)
		}
	}

	utils.InfoLog("Started new stream %s for user %s", streamID, username)
	return streamBuffer, nil
}

// newStreamClient allocates a client with its jitter buffer and done signal.
func newStreamClient() *streamClient {
	return &streamClient{
		ch:   make(chan []byte, clientBufferChunks),
		done: make(chan struct{}),
	}
}

// deliver enqueues a chunk for one client, applying back-pressure.
//
// When sole is true (the only viewer) it blocks until the client accepts the
// chunk or disconnects — reproducing a direct connection's TCP back-pressure.
// With multiple viewers it still blocks (so the pump tracks the slowest client),
// but a client whose buffer stays full past clientStallTimeout is dropped so it
// cannot freeze the shared upstream for everyone else. Returns true if dropped.
func (sm *SessionManager) deliver(buffer *StreamBuffer, cl *streamClient, chunk []byte, sole bool) (dropped bool) {
	if sole {
		select {
		case cl.ch <- chunk:
		case <-cl.done:
		case <-buffer.stopChan:
		}
		return false
	}
	select {
	case cl.ch <- chunk:
		return false
	case <-cl.done:
		return false
	case <-buffer.stopChan:
		return false
	case <-time.After(sm.clientStallTimeout):
		cl.close()
		return true
	}
}

// streamToClients pumps the single upstream connection and fans each chunk out
// to every attached client. It is the only reader of the upstream body.
func (sm *SessionManager) streamToClients(buffer *StreamBuffer, upstreamURL *url.URL) {
	utils.DebugLog("Starting stream from %s", upstreamURL.String())

	// Create a context that cancels when the stream is stopped
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-buffer.stopChan
		cancel()
	}()

	// Bind the upstream request to the cancelable context
	req, err := http.NewRequestWithContext(ctx, "GET", upstreamURL.String(), nil)
	if err != nil {
		utils.ErrorLog("Failed to create request: %v", err)
		return
	}

	// Set common headers; never inject Range — let the upstream return a natural 200
	// with Content-Length so clients can seek. Injecting bytes=0- forces a 206 response
	// which may lack a Content-Range total and causes avformat errors in media players.
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", utils.GetLanguageHeader())
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connection", "keep-alive")

	resp, err := sm.httpClient.Do(req)
	if err != nil {
		utils.ErrorLog("Failed to connect to upstream: %v", err)
		sm.stopStreamLocking(buffer.streamID)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Accept 200 (expected) and 206 (some providers return it unconditionally).
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		utils.ErrorLog("Upstream returned status %d for stream %s",
			resp.StatusCode, buffer.streamID)
		sm.stopStreamLocking(buffer.streamID)
		return
	}

	// The buffer was created active; the pump does not flip the flag itself so
	// that every read/write of buffer.active stays under sm.streamLock.
	dataBuffer := make([]byte, streamChunkSize)

	for {
		// Stop requested
		select {
		case <-buffer.stopChan:
			utils.DebugLog("Stream %s stopped", buffer.streamID)
			return
		default:
		}

		n, rerr := resp.Body.Read(dataBuffer)
		if n > 0 {
			// Each client reads concurrently, so the chunk must be its own copy
			// (dataBuffer is reused on the next read).
			chunk := make([]byte, n)
			copy(chunk, dataBuffer[:n])
			sm.fanOut(buffer, chunk)

			// Write to the disk buffer for catchup/timeshift playback.
			if buffer.diskBuffer != nil {
				if _, werr := buffer.diskBuffer.Write(chunk); werr != nil {
					utils.WarnLog("Catchup buffer write error for stream %s: %v", buffer.streamID, werr)
					// Non-fatal: disk errors must not kill the live stream for viewers.
				}
			}

			// Touch stream LastRequested to avoid cleanup timeout while data flows
			sm.streamLock.Lock()
			if ss, ok := sm.streamSessions[buffer.streamID]; ok {
				ss.LastRequested = time.Now()
			}
			sm.streamLock.Unlock()
		}
		if rerr != nil {
			if rerr != io.EOF && ctx.Err() == nil {
				utils.ErrorLog("Error reading from upstream: %v", rerr)
			}
			sm.stopStreamLocking(buffer.streamID)
			return
		}
	}
}

// fanOut delivers one chunk to every attached client in parallel and waits for
// all of them to accept it (or be dropped) before returning. This is what gives
// the pump its back-pressure: it cannot read the next upstream chunk until the
// slowest healthy client has taken the current one.
func (sm *SessionManager) fanOut(buffer *StreamBuffer, chunk []byte) {
	buffer.clientsLock.RLock()
	names := make([]string, 0, len(buffer.clients))
	targets := make([]*streamClient, 0, len(buffer.clients))
	for name, cl := range buffer.clients {
		names = append(names, name)
		targets = append(targets, cl)
	}
	buffer.clientsLock.RUnlock()

	if len(targets) == 0 {
		return
	}
	sole := len(targets) == 1

	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(name string, cl *streamClient) {
			defer wg.Done()
			if sm.deliver(buffer, cl, chunk, sole) {
				utils.WarnLog("Dropping slow client %s from stream %s (buffer stalled)", name, buffer.streamID)
				sm.RemoveClient(buffer.streamID, name)
			}
		}(names[i], targets[i])
	}
	wg.Wait()
}

// GetClientChannel retrieves the data channel for a specific client.
func (sm *SessionManager) GetClientChannel(streamID, username string) (chan []byte, bool) {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()

	buffer, exists := sm.streamBuffers[streamID]
	if !exists || !buffer.active {
		return nil, false
	}

	buffer.clientsLock.RLock()
	defer buffer.clientsLock.RUnlock()

	cl, exists := buffer.clients[username]
	if !exists {
		return nil, false
	}
	return cl.ch, true
}

// GetClientDone returns the termination signal for a specific client. The HTTP
// handler selects on it so it exits when the pump drops the client (slow viewer)
// or the stream stops.
func (sm *SessionManager) GetClientDone(streamID, username string) (<-chan struct{}, bool) {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()

	buffer, exists := sm.streamBuffers[streamID]
	if !exists || !buffer.active {
		return nil, false
	}

	buffer.clientsLock.RLock()
	defer buffer.clientsLock.RUnlock()

	cl, exists := buffer.clients[username]
	if !exists {
		return nil, false
	}
	return cl.done, true
}

// RemoveClient removes a client from a stream.
func (sm *SessionManager) RemoveClient(streamID, username string) {
	// Clear the user session first, then take streamLock. This respects the
	// documented lock order (userLock → streamLock) and never holds both at
	// once, so it cannot deadlock against cleanupExpiredSessions.
	sm.userLock.Lock()
	if userSession, exists := sm.userSessions[username]; exists && userSession.StreamID == streamID {
		userSession.StreamID = ""
		userSession.StreamType = ""
	}
	sm.userLock.Unlock()

	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()

	// Signal the client's HTTP handler to finish, then detach it.
	buffer, exists := sm.streamBuffers[streamID]
	if !exists {
		return
	}

	buffer.clientsLock.Lock()
	if cl, ok := buffer.clients[username]; ok {
		cl.close()
		delete(buffer.clients, username)
	}
	buffer.clientsLock.Unlock()

	// Remove from stream session and stop the stream if last viewer
	streamSession, exists := sm.streamSessions[streamID]
	if !exists {
		return
	}
	if !streamSession.RemoveViewer(username) && buffer.active {
		sm.stopStream(streamID)
	}

	utils.InfoLog("User %s removed from stream %s", username, streamID)
}

// stopStreamLocking acquires streamLock and stops the stream. Used by the
// upstream pump goroutine, which does not otherwise hold the lock.
func (sm *SessionManager) stopStreamLocking(streamID string) {
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()
	sm.stopStream(streamID)
}

// stopStream stops an active stream and disconnects all of its clients.
// The caller must hold sm.streamLock.
func (sm *SessionManager) stopStream(streamID string) {
	utils.InfoLog("Stopping stream %s", streamID)

	buffer, exists := sm.streamBuffers[streamID]
	if !exists || !buffer.active {
		return
	}

	// Signal the upstream pump to stop (Once prevents double-close panic)
	buffer.stopOnce.Do(func() { close(buffer.stopChan) })
	buffer.active = false

	// Signal every client's HTTP handler to finish
	buffer.clientsLock.Lock()
	for _, cl := range buffer.clients {
		cl.close()
	}
	buffer.clients = make(map[string]*streamClient)
	buffer.clientsLock.Unlock()

	// Delete the disk buffer immediately. On Linux, open file handles held by
	// in-flight timeshift readers keep the inode alive until they close.
	if buffer.diskBuffer != nil {
		sm.catchupManager.DeleteBuffer(streamID)
		buffer.diskBuffer = nil
	}

	// Update the stream session
	if streamSession, exists := sm.streamSessions[streamID]; exists {
		streamSession.Active = false
	}

	utils.InfoLog("Stream %s stopped and all clients disconnected", streamID)
}

// GenerateTemporaryLink creates a temporary download link
func (sm *SessionManager) GenerateTemporaryLink(username, streamID, title, rawURL string) (string, error) {
	token := uuid.New().String()
	expiresAt := time.Now().Add(sm.tempLinkTimeout)

	tempLink := &types.TemporaryLink{
		Token:     token,
		Username:  username,
		URL:       rawURL,
		ExpiresAt: expiresAt,
		StreamID:  streamID,
		Title:     title,
	}

	// Store in memory
	sm.tempLinkLock.Lock()
	sm.tempLinks[token] = tempLink
	sm.tempLinkLock.Unlock()

	// Store in database if available
	if sm.db != nil {
		if err := sm.db.CreateTemporaryLink(token, username, rawURL, streamID, title, expiresAt); err != nil {
			utils.ErrorLog("Failed to store temporary link in database: %v", err)
		}
	}

	utils.InfoLog("Generated temporary link for user %s, expires at %v", username, expiresAt)
	return token, nil
}

// GetTemporaryLink retrieves a temporary link by token
func (sm *SessionManager) GetTemporaryLink(token string) (*types.TemporaryLink, error) {
	// First check in memory
	sm.tempLinkLock.RLock()
	tempLink, exists := sm.tempLinks[token]
	sm.tempLinkLock.RUnlock()

	if exists && time.Now().Before(tempLink.ExpiresAt) {
		return tempLink, nil
	}

	// If not in memory or expired, try the database
	if sm.db != nil {
		return sm.db.GetTemporaryLink(token)
	}

	return nil, fmt.Errorf("temporary link not found or expired")
}

// GetAllSessions returns all current user sessions
func (sm *SessionManager) GetAllSessions() []*types.UserSession {
	sm.userLock.RLock()
	defer sm.userLock.RUnlock()

	sessions := make([]*types.UserSession, 0, len(sm.userSessions))
	for _, session := range sm.userSessions {
		sessions = append(sessions, session)
	}

	return sessions
}

// GetAllStreams returns all active stream sessions
func (sm *SessionManager) GetAllStreams() []*types.StreamSession {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()

	streams := make([]*types.StreamSession, 0, len(sm.streamSessions))
	for _, stream := range sm.streamSessions {
		if stream.Active {
			streams = append(streams, stream)
		}
	}

	return streams
}

// DisconnectUser forcibly disconnects all streams for a user
func (sm *SessionManager) DisconnectUser(username string) {
	sm.userLock.Lock()
	userSession, exists := sm.userSessions[username]
	if !exists {
		sm.userLock.Unlock()
		return
	}

	streamID := userSession.StreamID
	userSession.StreamID = ""
	userSession.StreamType = ""
	sm.userLock.Unlock()

	// If user was watching a stream, remove them
	if streamID != "" {
		sm.RemoveClient(streamID, username)
	}

	utils.InfoLog("User %s forcibly disconnected", username)
}

// RegisterVODView creates a synthetic stream session so status commands see users watching local files.
func (sm *SessionManager) RegisterVODView(username, streamID, streamType, title string) {
	sm.userLock.Lock()
	if sess, exists := sm.userSessions[username]; exists {
		sess.StreamID = streamID
		sess.StreamType = streamType
		sess.LastActive = time.Now()
	}
	sm.userLock.Unlock()

	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()
	if ss, exists := sm.streamSessions[streamID]; exists {
		ss.AddViewer(username)
		ss.LastRequested = time.Now()
	} else {
		ss := &types.StreamSession{
			StreamID: streamID, StreamType: streamType, StreamTitle: title,
			StartTime: time.Now(), LastRequested: time.Now(),
			Viewers: make(map[string]time.Time), Active: true,
		}
		ss.AddViewer(username)
		sm.streamSessions[streamID] = ss
	}
}

// UnregisterVODView removes a user from a synthetic VOD viewing session.
func (sm *SessionManager) UnregisterVODView(username, streamID string) {
	sm.userLock.Lock()
	if sess, exists := sm.userSessions[username]; exists && sess.StreamID == streamID {
		sess.StreamID = ""
		sess.StreamType = ""
	}
	sm.userLock.Unlock()

	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()
	if ss, exists := sm.streamSessions[streamID]; exists {
		if !ss.RemoveViewer(username) {
			ss.Active = false
			delete(sm.streamSessions, streamID)
		}
	}
}

// GetStreamInfo gets information about a specific stream
func (sm *SessionManager) GetStreamInfo(streamID string) (*types.StreamSession, bool) {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()

	session, exists := sm.streamSessions[streamID]
	return session, exists
}

// SetSessionTimeout sets the user session timeout duration
func (sm *SessionManager) SetSessionTimeout(timeout time.Duration) {
	sm.sessionTimeout = timeout
}

// SetStreamTimeout sets the unused stream timeout duration
func (sm *SessionManager) SetStreamTimeout(timeout time.Duration) {
	sm.streamTimeout = timeout
}

// SetTempLinkTimeout sets the temporary link expiration duration
func (sm *SessionManager) SetTempLinkTimeout(timeout time.Duration) {
	sm.tempLinkTimeout = timeout
}

// SetVODCacheStaleAge sets how long a cached file can go unaccessed before cleanup.
func (sm *SessionManager) SetVODCacheStaleAge(d time.Duration) {
	sm.vodCacheStaleAge = d
}

// SetClientStallTimeout sets how long the pump waits on a multiplexed client
// whose buffer is full before dropping it (to protect the other viewers).
func (sm *SessionManager) SetClientStallTimeout(d time.Duration) {
	if d > 0 {
		sm.clientStallTimeout = d
	}
}

// SetCatchupManager wires a catchup.Manager into the session manager so that
// live streams are buffered to disk for local timeshift playback.
func (sm *SessionManager) SetCatchupManager(m *catchup.Manager) {
	sm.catchupManager = m
}

// cleanupStaleVODFiles deletes cached VOD files (and their DB rows) that have
// not been accessed within vodCacheStaleAge. In-progress downloads are skipped.
func (sm *SessionManager) cleanupStaleVODFiles() {
	cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
	if cacheDir == "" {
		cacheDir = os.TempDir()
	}
	cacheDir = filepath.Clean(cacheDir)

	threshold := time.Now().Add(-sm.vodCacheStaleAge)
	entries, err := sm.db.GetStaleVODCache(threshold)
	if err != nil {
		utils.ErrorLog("Failed to query stale VOD cache: %v", err)
		return
	}
	for _, e := range entries {
		// Safety: only delete files that are inside the expected cache directory.
		if !strings.HasPrefix(filepath.Clean(e.FilePath), cacheDir+string(os.PathSeparator)) {
			utils.WarnLog("Refusing to delete out-of-cache-dir path: %s", e.FilePath)
			continue
		}
		if err := os.Remove(e.FilePath); err != nil && !os.IsNotExist(err) {
			utils.WarnLog("Could not delete stale VOD file %s: %v", e.FilePath, err)
		}
		if err := sm.db.DeleteVODCacheEntry(e.StreamID); err != nil {
			utils.ErrorLog("Failed to remove stale VOD cache row for %s: %v", e.StreamID, err)
		} else {
			utils.InfoLog("Removed stale VOD cache entry %s (last accessed %s ago)", e.StreamID, utils.HumanDuration(time.Since(e.LastAccess)))
		}
	}
}
