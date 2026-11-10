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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/discord"
	"github.com/lucasduport/stream-share/pkg/filter"
	"github.com/lucasduport/stream-share/pkg/provider"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/slate"
	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// upstreamReadyTimeout bounds how long a viewer waits for the provider to
// answer before we give up and return a status. It has to stay tight: this sits
// in the channel-zap path, so a generous value makes every start feel sluggish
// when the provider is slow.
const upstreamReadyTimeout = 8 * time.Second

var defaultProxyfiedM3UPath = filepath.Join(os.TempDir(), uuid.New().String()+".stream-share.m3u")
var endpointAntiColision = strings.Split(uuid.New().String(), "-")[0]

// Config represents all server dependencies and runtime configuration.
type Config struct {
	*config.ProxyConfig

	// M3U service part
	playlist *m3u.Playlist
	// this variable is set only for m3u proxy endpoints
	track *m3u.Track
	// path to the proxyfied m3u file
	proxyfiedM3UPath string

	endpointAntiColision string

	// New components
	sessionManager *session.SessionManager
	catchupManager *catchup.Manager
	db             *database.DBManager
	discordBot     *discord.Bot

	// startTime records process start, used to report uptime via the API
	startTime time.Time

	// imageKey signs /img links (see imageSignature)
	imageKey []byte

	// ready flips to 1 once startup completes and the server is about to listen,
	// so /healthz can report container readiness for `depends_on:
	// condition: service_healthy` ordering. Accessed atomically (0 = starting).
	ready int32

	// health caches the latest provider health-probe result. Non-nil only when
	// HealthCheckEnabled; nil means /healthz reports "disabled".
	health *healthState

	// providerInfo caches the upstream provider's subscription state. Non-nil
	// only when an Xtream provider is configured; nil means the concept does not
	// apply to this deployment (plain M3U) and the endpoint reports so.
	providerInfo *providerInfoState

	// inProgressDownloads holds the running VOD cache downloads (stream ID ->
	// *vodDownload): one per item, shared by every request served from it.
	inProgressDownloads sync.Map

	// providerSlots arbitrates the media connections opened to the provider
	// (see providerSlotCapacity); never nil once the server is built.
	providerSlots *provider.Arbiter

	// shares holds the live share links (see live_shares.go).
	shares *shareStore

	// catalogFilter is the compiled catalogue filter; nil keeps everything, and
	// every filtering path checks for nil first so it costs nothing when unset.
	catalogFilter *filter.Set
}

// NewServer initializes a new server configuration with all necessary components.
func NewServer(config *config.ProxyConfig) (*Config, error) {
	utils.RegisterSecrets(config.XtreamUser.String(), config.XtreamPassword.String(), config.User.String(), config.Password.String())
	config.CatalogStorage = parseCatalogStorage(config.CatalogStorage)
	applyCatalogMemoryLimit(config.CatalogStorage)
	var p m3u.Playlist

	// Parse the M3U playlist from the remote URL if provided — unless it is the
	// Xtream account's own get.php, which is then served by the streaming
	// get.php proxy instead. Parsing it here would download the whole playlist
	// before the server can start and keep every track in memory for nothing
	// (about 1.3 GiB of heap and minutes of boot on a million-track catalogue).
	if config.RemoteURL.String() != "" {
		if (&Config{ProxyConfig: config}).xtreamServesM3U() {
			utils.InfoLog("M3U_URL is the Xtream account's get.php: serving it through the Xtream proxy, not parsing it at boot")
		} else {
			var err error
			p, err = m3u.Parse(config.RemoteURL.String())
			if err != nil {
				return nil, utils.PrintErrorAndReturn(err)
			}
			utils.InfoLog("Successfully parsed M3U playlist from %s", utils.MaskURL(config.RemoteURL.String()))
		}
	}

	catalogFilter, err := filter.NewSet(config.CatalogFilter)
	if err != nil {
		return nil, utils.PrintErrorAndReturn(err)
	}
	if catalogFilter.Active() {
		utils.InfoLog("Catalogue filter enabled (rules %s)", catalogFilter.Fingerprint())
	}

	// Use custom ID for endpoint if provided, otherwise use a generated one
	customID := endpointAntiColision
	if trimmedCustomId := strings.Trim(config.CustomId, "/"); trimmedCustomId != "" {
		customID = trimmedCustomId
		utils.InfoLog("Using custom endpoint ID: %s", customID)
	}

	// Pin the internal API key from configuration if provided
	SetAPIKey(config.InternalAPIKey)

	// Create server configuration
	serverConfig := &Config{
		ProxyConfig:          config,
		playlist:             &p,
		track:                nil,
		proxyfiedM3UPath:     defaultProxyfiedM3UPath,
		endpointAntiColision: customID,
		sessionManager:       nil,
		db:                   nil,
		discordBot:           nil,
		startTime:            time.Now(),
		catalogFilter:        catalogFilter,
		imageKey:             loadImageSigningKey(utils.CacheRoot()),
	}

	// Provider health check: report-only. Reconnecting the VPN on a bad result is
	// left to an external watchdog (see docker-compose.yml), so nothing here knows
	// about gluetun or any VPN.
	if config.HealthCheckEnabled {
		if config.HealthCheckTimeoutSeconds <= 0 {
			config.HealthCheckTimeoutSeconds = 15
		}
		if config.HealthCheckMinIntervalSeconds <= 0 {
			config.HealthCheckMinIntervalSeconds = 60
		}
		if strings.TrimSpace(config.HealthCheckStreamID) == "" {
			utils.WarnLog("Health check enabled but HEALTHCHECK_STREAM_ID is empty; probes will report 'error'")
		}
		serverConfig.health = &healthState{}
	}

	// Subscription info comes from the provider's own login response, so it only
	// exists for Xtream-backed deployments.
	if strings.TrimSpace(config.XtreamBaseURL) != "" {
		serverConfig.providerInfo = &providerInfoState{}
	}
	serverConfig.providerSlots = provider.New(serverConfig.providerSlotCapacity)
	serverConfig.shares = newShareStore()

	// Force PostgreSQL initialization (sqlite removed)
	utils.InfoLog("Bootstrap: Forcing PostgreSQL database initialization")
	db, err := database.NewDBManager("") // path unused for postgres
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}
	serverConfig.db = db
	serverConfig.sessionManager = session.NewSessionManager(db)
	serverConfig.sessionManager.SetNameResolver(serverConfig.resolveStreamName)
	serverConfig.sessionManager.SetProviderSlots(serverConfig.providerSlots)
	utils.InfoLog("Session manager initialized with database connection")

	// Seed in-memory name indices from DB so names are available before the first API call.
	serverConfig.warmChannelIndexFromDB()

	// After session manager init
	if serverConfig.sessionManager == nil {
		utils.ErrorLog("Bootstrap: sessionManager is NIL - multiplexing will NOT be used")
	} else {
		utils.InfoLog("Bootstrap: sessionManager initialized OK")
	}

	// Initialize local catchup buffering from configuration
	catchupEnabled := config.CatchupEnabled
	catchupDir := utils.CatchupBufferDir()
	catchupDur := 4
	if config.CatchupDurationHours > 0 {
		catchupDur = config.CatchupDurationHours
	}
	serverConfig.catchupManager = catchup.New(catchupEnabled, catchupDir, catchupDur)
	if catchupEnabled {
		serverConfig.catchupManager.CleanupOldFiles()
		tz := os.Getenv("TZ")
		if tz == "" {
			utils.WarnLog("Bootstrap: catchup is ENABLED but TZ env var is not set — timeshift timestamps from clients will be parsed as UTC and rewinds will land at the wrong position. Set TZ to your clients' timezone (e.g. TZ=Europe/Amsterdam).")
		} else {
			utils.InfoLog("Bootstrap: local catchup buffering ENABLED (dir=%s, duration=%dh, TZ=%s)", catchupDir, catchupDur, tz)
		}
	} else {
		utils.InfoLog("Bootstrap: local catchup buffering DISABLED (set CATCHUP_ENABLED=true to enable)")
	}
	if serverConfig.sessionManager != nil {
		serverConfig.sessionManager.SetCatchupManager(serverConfig.catchupManager)
	}

	// Pause grace: how long a catchup-enabled live stream stays alive (upstream
	// connection open, disk recording continuing) after its last viewer
	// disconnects, so a TiviMate "pause" followed by a timeshift-based resume
	// has continuous buffered content with no gap. Channel switches are detected
	// separately and bypass this grace period (see SessionManager.RequestStream).
	catchupPauseGrace := 5
	if config.CatchupPauseGraceMinutes >= 0 {
		catchupPauseGrace = config.CatchupPauseGraceMinutes
	}
	if serverConfig.sessionManager != nil {
		serverConfig.sessionManager.SetPauseGrace(time.Duration(catchupPauseGrace) * time.Minute)
		if catchupEnabled && catchupPauseGrace > 0 {
			utils.InfoLog("Bootstrap: catchup pause grace set to %d minute(s) — paused live streams keep recording for seamless resume", catchupPauseGrace)
		}
	}

	// Error slate: show viewers why a live stream failed instead of dropping the
	// connection, and keep retrying the provider behind the slate. Requires
	// ffmpeg; when it is missing the generator reports unavailable and the
	// session manager silently keeps the previous drop behavior, so older images
	// are unaffected by the default being on.
	if serverConfig.sessionManager != nil {
		slateRetry := 10
		if config.ErrorSlateRetryMaxMinutes > 0 {
			slateRetry = config.ErrorSlateRetryMaxMinutes
		}
		serverConfig.sessionManager.SetErrorCatalog(session.LoadCatalog(config.ErrorSlateMessagesFile))

		if config.ErrorSlateEnabled {
			gen := slate.New(utils.SlateCacheDir())
			serverConfig.sessionManager.SetSlateProvider(gen)
			serverConfig.sessionManager.SetSlateRetryMax(time.Duration(slateRetry) * time.Minute)
			if gen.Available() {
				utils.InfoLog("Bootstrap: error slates ENABLED (retry up to %d minute(s) before giving up)", slateRetry)
			} else {
				utils.WarnLog("Bootstrap: error slates requested but ffmpeg is unavailable; streams will drop on upstream failure as before")
			}
		} else {
			utils.InfoLog("Bootstrap: error slates DISABLED (set ERROR_SLATE_ENABLED=true to show upstream errors on screen)")
		}
	}

	// The LDAP cache trades revocation latency for a large drop in directory
	// traffic, so make the active window visible at startup rather than leaving
	// an operator to infer it.
	if config.LDAPEnabled {
		if mins := config.LDAPAuthCacheMinutes; mins > 0 {
			utils.InfoLog("Bootstrap: LDAP auth cache ENABLED (successful logins trusted for %d minute(s); disabled accounts keep working until their entry expires)", mins)
		} else {
			utils.InfoLog("Bootstrap: LDAP auth cache DISABLED — every request re-checks the directory")
		}
	}

	// Configure session parameters from configuration. Each setter is only
	// applied when the value is > 0, leaving the manager defaults otherwise.
	if serverConfig.sessionManager != nil {
		if mins := config.SessionTimeoutMinutes; mins > 0 {
			serverConfig.sessionManager.SetSessionTimeout(time.Duration(mins) * time.Minute)
			utils.InfoLog("Session timeout set to %d minutes", mins)
		}
		if mins := config.StreamTimeoutMinutes; mins > 0 {
			serverConfig.sessionManager.SetStreamTimeout(time.Duration(mins) * time.Minute)
			utils.InfoLog("Stream timeout set to %d minutes", mins)
		}
		if hours := config.TempLinkHours; hours > 0 {
			serverConfig.sessionManager.SetTempLinkTimeout(time.Duration(hours) * time.Hour)
			utils.InfoLog("Temporary link timeout set to %d hours", hours)
		}
		if hours := config.VODCacheStaleHours; hours > 0 {
			serverConfig.sessionManager.SetVODCacheStaleAge(time.Duration(hours) * time.Hour)
			utils.InfoLog("VOD cache stale age set to %d hours", hours)
		}
		// Prune the on-disk slate clip cache too (clips are regenerated on demand).
		// This runs regardless of ErrorSlateEnabled so leftover clips are cleaned up
		// even after the feature is turned off. 0 = default 7 days; negative disables.
		if hours := config.SlateCacheStaleHours; hours >= 0 {
			if hours == 0 {
				hours = 168
			}
			serverConfig.sessionManager.SetSlateCache(utils.SlateCacheDir(), time.Duration(hours)*time.Hour)
			utils.InfoLog("Slate cache stale age set to %d hours", hours)
		}
		if secs := config.MultiplexStallTimeoutSeconds; secs > 0 {
			serverConfig.sessionManager.SetClientStallTimeout(time.Duration(secs) * time.Second)
			utils.InfoLog("Multiplex client stall timeout set to %d seconds", secs)
		}
	}

	// Initialize Discord bot if token is provided
	discordToken := config.DiscordBotToken
	if discordToken != "" {
		utils.InfoLog("Initializing Discord bot")
		discordAdminRole := config.DiscordAdminRoleID

		// Get API URL from config, defaulting to host/port, but honor reverse proxy
		apiURL := config.DiscordAPIURL
		if apiURL == "" {
			// The bot runs in this same process and calls the local API, which only
			// serves plain HTTP on the listening port. Default to loopback so it
			// works regardless of the public scheme/host: a TLS-terminating proxy
			// in front would make a public HTTPS URL unreachable from here. Set
			// DISCORD_API_URL to override (e.g. when the bot runs elsewhere).
			apiURL = fmt.Sprintf("http://127.0.0.1:%d", config.HostConfig.Port)
		}
		utils.InfoLog("Discord API URL used by bot: %s", apiURL)
		utils.InfoLog("Reminder: Ensure 'MESSAGE CONTENT INTENT' is enabled in Discord Developer Portal for this bot.")

		bot, err := discord.NewBot(discordToken, discordAdminRole, apiURL, GetAPIKey())
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Discord bot: %w", err)
		}

		serverConfig.discordBot = bot
	} else {
		utils.InfoLog("Bootstrap: DISCORD_BOT_TOKEN not set - Discord bot is DISABLED")
	}

	// Remove debug API JSON dumps left over from previous runs when debug logging
	// is off. These files accumulate whenever CACHE_FOLDER is set and are only
	// useful in debug mode.
	if !utils.IsDebugLogEnabled() {
		if cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER")); cacheDir != "" {
			cleanDebugAPIFiles(cacheDir)
		}
	}

	return serverConfig, nil
}

// cleanDebugAPIFiles removes timestamped JSON debug dumps written by the
// player_api handler from the cache directory. VOD media files are unaffected.
func cleanDebugAPIFiles(cacheDir string) {
	patterns := []string{
		"login_????????_??????.json",
		"get_*_????????_??????.json",
	}
	removed := 0
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(cacheDir, pattern))
		for _, f := range matches {
			if err := os.Remove(f); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		utils.InfoLog("Cleaned %d debug API JSON file(s) from cache folder", removed)
	}
}

// Serve boots the HTTP server, internal API, routes, and optional Discord bot.
func (c *Config) Serve() error {
	utils.InfoLog("[stream-share] Server is starting...")

	switch {
	case c.db != nil && c.db.IsInitialized():
		utils.InfoLog("Bootstrap: Database is initialized and connected")
	case c.db != nil:
		utils.WarnLog("Bootstrap: Database manager present but not initialized")
	default:
		utils.WarnLog("Bootstrap: Database is DISABLED (no persistence)")
	}

	if c.sessionManager == nil {
		utils.ErrorLog("Bootstrap: sessionManager is NIL inside Serve()")
	}

	if err := c.playlistInitialization(); err != nil {
		utils.ErrorLog("Playlist initialization failed: %v", err)
		return err
	}

	// Warm the channel-name index from get_live_streams so /status and logs can
	// resolve names immediately, without waiting for a player to request the list.
	go c.warmChannelNameIndex()

	// Start the VOD M3U download now: on big catalogues it takes minutes, and
	// lookups never wait for it, so starting early is what makes extensions and
	// titles available by the time the first VOD is requested.
	if c.XtreamBaseURL != "" {
		_, _ = c.ensureVODM3UCache()
		// Same for the XMLTV guide, which players fetch right after login.
		c.ensureEPGCache()
	}

	// Start background goroutine to keep apiChannelIndex fresh.
	nameRefreshStop := make(chan struct{})
	c.startNameIndexRefresher(nameRefreshStop)
	defer close(nameRefreshStop)

	// Keep the provider subscription snapshot warm so dashboard requests are
	// always served from cache instead of waiting on the provider.
	providerInfoStop := make(chan struct{})
	c.startProviderInfoRefresher(providerInfoStop)

	// Share links: load them, and cut viewers of links that end or are revoked.
	c.loadLiveShares()
	shareSweepStop := make(chan struct{})
	c.startLiveShareSweeper(shareSweepStop)
	defer close(shareSweepStop)
	defer close(providerInfoStop)

	// Drop expired LDAP auth-cache entries so they do not linger for the life of
	// the process. Only successes are cached, so the map is small either way.
	if c.LDAPEnabled && c.ldapCacheTTL() > 0 {
		ldapSweepStop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(c.ldapCacheTTL())
			defer ticker.Stop()
			for {
				select {
				case <-ldapSweepStop:
					return
				case <-ticker.C:
					sweepLDAPCache()
				}
			}
		}()
		defer close(ldapSweepStop)
	}

	recentAuthSweepStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-recentAuthSweepStop:
				return
			case now := <-ticker.C:
				sweepRecentAuth(now)
			}
		}
	}()
	defer close(recentAuthSweepStop)

	if c.sessionManager != nil {
		defer c.sessionManager.Stop()
	}
	if c.catchupManager != nil {
		defer c.catchupManager.Cleanup()
	}

	// Start Discord bot if configured
	if c.discordBot != nil {
		utils.InfoLog("Starting Discord bot...")
		if err := c.discordBot.Start(); err != nil {
			return fmt.Errorf("failed to start Discord bot: %w", err)
		}
		defer c.discordBot.Stop()
	}

	// gin.New() rather than gin.Default(): the default access logger logs every
	// request, which drowns the log in noise from the frequently-polled /healthz
	// and internal API endpoints. Log only failures (>= 400) unless debug logging
	// is on, in which case successful requests are logged too, credentials redacted.
	// Release mode always (unless GIN_MODE is set): in debug mode gin prints
	// every route at startup straight to stdout, bypassing log redaction, and
	// the Xtream stream routes embed the provider's username and password.
	if os.Getenv(gin.EnvGinMode) == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.New()
	if err := c.applyTrustedProxies(router); err != nil {
		return fmt.Errorf("invalid trusted proxy list: %w", err)
	}
	router.Use(c.quietRequestLogger())
	router.Use(gin.Recovery())
	router.Use(cors.Default())
	utils.InfoLog("Setting up routes and internal API...")

	// Setup API routes for Discord bot and other internal tools
	c.setupInternalAPI(router)

	// Setup regular routes
	group := router.Group("/")
	c.routes(group)

	// Add direct streaming routes with proxy credentials
	c.addProxyCredentialRoutes(router)

	// Add temporary link download route
	router.GET("/download/:token", c.handleTemporaryLink)

	// Live share links: the token is the credential (see live_shares.go).
	router.GET("/share/:token", c.handleSharePlaylist)
	router.GET("/share/:token/live.ts", c.handleShareStream)

	// Container readiness endpoint for the Docker HEALTHCHECK. Reports whether
	// THIS service has finished starting (see the ready flag set below), never
	// provider/VPN state, so it is safe to gate compose ordering on.
	router.GET("/healthz", c.healthz)

	// Seed and, if configured, schedule provider health probes.
	if c.health != nil {
		healthStop := make(chan struct{})
		c.startHealthMonitor(healthStop)
		defer close(healthStop)
	}

	// Startup is complete; mark the service ready so /healthz returns 200 (and the
	// container reports healthy) from here on. router.Run below starts accepting
	// connections, so any request that reaches /healthz is necessarily past this
	// point.
	atomic.StoreInt32(&c.ready, 1)

	// Add a message to indicate the server is ready
	utils.InfoLog("[stream-share] Server is ready and listening on :%d", c.HostConfig.Port)
	return router.Run(fmt.Sprintf(":%d", c.HostConfig.Port))
}

// startNameIndexRefresher runs a background goroutine that periodically re-fetches
// get_live_streams from the upstream Xtream API to keep apiChannelIndex fresh.
func (c *Config) startNameIndexRefresher(stopCh <-chan struct{}) {
	interval := time.Duration(c.M3UCacheExpiration) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				c.refreshAPIChannelIndex()
			}
		}
	}()
}

// refreshAPIChannelIndex re-fetches get_live_streams from the upstream Xtream API
// and updates the in-memory apiChannelIndex (and persists to DB). The catalog
// fetch goes through the shared cache, so overlapping refreshes and player
// fetches coalesce into a single provider round-trip.
func (c *Config) refreshAPIChannelIndex() {
	if c.XtreamBaseURL == "" {
		return
	}
	// Give a cold or never-warmed category index another chance every refresh
	// cycle, not just at process startup — otherwise a single get_live_categories
	// failure at boot leaves categories empty for the process's whole lifetime.
	if client, cerr := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, ""); cerr == nil {
		c.warmCategoryNameIndex(client)
	} else {
		utils.WarnLog("stream_names refresh: failed to create Xtream client: %v", cerr)
	}
	// Force a provider round-trip; on failure the cache keeps (and players
	// keep getting) the last good catalogue.
	e, stale, err := fetchCatalogEntry(c.catalogKey("get_live_streams"), true, c.catalogFetcher("get_live_streams", nil))
	if err != nil || stale {
		utils.WarnLog("stream_names refresh: get_live_streams failed: %v", err)
		return
	}
	if n := c.harvestChannelNames(e.data); n > 0 {
		utils.DebugLog("stream_names: refreshed %d live channel names from upstream", n)
	}
}
