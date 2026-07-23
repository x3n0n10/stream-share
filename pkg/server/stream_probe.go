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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// Technical stream info is derived from a small sample of bytes already
// flowing through an active live stream's shared upstream connection (see
// SessionManager.CaptureStreamSample) — probing never opens an extra
// connection to the provider. The sample is analyzed locally with ffprobe.
const (
	techProbeSampleBytes    = 2 * 1024 * 1024 // ~2MB, enough for ffprobe to see a keyframe + audio frames on typical bitrates
	techProbeCaptureTimeout = 6 * time.Second
	techProbeFFprobeTimeout = 6 * time.Second
	techProbeCacheTTL       = 5 * time.Minute
	techProbeFailureTTL     = time.Minute // retry failed probes sooner than successful ones
)

// StreamTechInfo describes the audio/video technical characteristics of an
// active live stream, as last observed by ffprobe.
type StreamTechInfo struct {
	ContainerFormat  string    `json:"container_format,omitempty"`
	VideoCodec       string    `json:"video_codec,omitempty"`
	Width            int       `json:"width,omitempty"`
	Height           int       `json:"height,omitempty"`
	FrameRate        float64   `json:"frame_rate,omitempty"`
	VideoBitrateKbps int       `json:"video_bitrate_kbps,omitempty"`
	AudioCodec       string    `json:"audio_codec,omitempty"`
	AudioChannels    int       `json:"audio_channels,omitempty"`
	AudioSampleRate  int       `json:"audio_sample_rate_hz,omitempty"`
	AudioLanguage    string    `json:"audio_language,omitempty"`
	AudioBitrateKbps int       `json:"audio_bitrate_kbps,omitempty"`
	ProbedAt         time.Time `json:"probed_at,omitempty"`
	Error            string    `json:"error,omitempty"`
}

type techCacheEntry struct {
	info      *StreamTechInfo
	expiresAt time.Time
}

var (
	techCacheMu sync.Mutex
	techCache   = map[string]*techCacheEntry{} // streamID -> cached result
	techProbing = map[string]bool{}            // streamID -> probe in flight

	ffprobeCheckOnce sync.Once
	ffprobeOK        bool
)

// ffprobeAvailable reports whether the ffprobe binary is on PATH, checked once.
func ffprobeAvailable() bool {
	ffprobeCheckOnce.Do(func() {
		path, err := exec.LookPath("ffprobe")
		ffprobeOK = err == nil
		if ffprobeOK {
			utils.InfoLog("Stream tech probe: ffprobe found at %s", path)
		} else {
			utils.WarnLog("Stream tech probe: ffprobe not found on PATH; technical stream info will be unavailable")
		}
	})
	return ffprobeOK
}

// getCachedTechInfo returns a previously probed result without triggering any
// work. staleness is not considered here — callers decide whether to also warm.
func getCachedTechInfo(streamID string) (*StreamTechInfo, bool) {
	techCacheMu.Lock()
	defer techCacheMu.Unlock()
	entry, ok := techCache[streamID]
	if !ok {
		return nil, false
	}
	return entry.info, true
}

// warmTechInfo kicks off a background probe for streamID if the feature is
// enabled, ffprobe is available, and there isn't already a fresh cache entry
// or an in-flight probe for it. Never blocks the caller.
func (c *Config) warmTechInfo(streamID string) {
	if !c.StreamTechProbeEnabled || !ffprobeAvailable() {
		return
	}

	techCacheMu.Lock()
	if techProbing[streamID] {
		techCacheMu.Unlock()
		return
	}
	if entry, ok := techCache[streamID]; ok && time.Now().Before(entry.expiresAt) {
		techCacheMu.Unlock()
		return
	}
	techProbing[streamID] = true
	techCacheMu.Unlock()

	go func() {
		defer func() {
			techCacheMu.Lock()
			delete(techProbing, streamID)
			techCacheMu.Unlock()
		}()
		c.probeAndCacheTechInfo(streamID)
	}()
}

// forceProbeTechInfo runs a probe synchronously (used for single-stream
// lookups, where a caller can afford to wait a couple of seconds for a fresh
// result) and returns whatever ends up cached, including the last-good result
// if this attempt fails.
func (c *Config) forceProbeTechInfo(streamID string) *StreamTechInfo {
	if !c.StreamTechProbeEnabled || !ffprobeAvailable() {
		return nil
	}

	techCacheMu.Lock()
	alreadyProbing := techProbing[streamID]
	if !alreadyProbing {
		techProbing[streamID] = true
	}
	techCacheMu.Unlock()

	if !alreadyProbing {
		defer func() {
			techCacheMu.Lock()
			delete(techProbing, streamID)
			techCacheMu.Unlock()
		}()
		c.probeAndCacheTechInfo(streamID)
	}

	info, _ := getCachedTechInfo(streamID)
	return info
}

// probeAndCacheTechInfo samples the stream's already-flowing upstream bytes,
// analyzes them with ffprobe, and stores the result (success or failure) in
// the cache. Caller is responsible for the techProbing in-flight guard.
func (c *Config) probeAndCacheTechInfo(streamID string) {
	sample, err := c.sessionManager.CaptureStreamSample(streamID, techProbeSampleBytes, techProbeCaptureTimeout)
	if err != nil {
		utils.DebugLog("Stream tech probe: sample capture failed for %s: %v", streamID, err)
		cacheTechInfo(streamID, &StreamTechInfo{Error: err.Error()}, techProbeFailureTTL)
		return
	}

	info, err := runFFprobeOnSample(sample)
	if err != nil {
		utils.DebugLog("Stream tech probe: ffprobe failed for %s: %v", streamID, err)
		cacheTechInfo(streamID, &StreamTechInfo{Error: err.Error()}, techProbeFailureTTL)
		return
	}
	info.ProbedAt = time.Now()
	cacheTechInfo(streamID, info, techProbeCacheTTL)
}

func cacheTechInfo(streamID string, info *StreamTechInfo, ttl time.Duration) {
	techCacheMu.Lock()
	techCache[streamID] = &techCacheEntry{info: info, expiresAt: time.Now().Add(ttl)}
	techCacheMu.Unlock()
}

// ffprobeStream is the subset of ffprobe's per-stream JSON output we care about.
type ffprobeStream struct {
	CodecType    string `json:"codec_type"`
	CodecName    string `json:"codec_name"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	AvgFrameRate string `json:"avg_frame_rate"`
	RFrameRate   string `json:"r_frame_rate"`
	BitRate      string `json:"bit_rate"`
	Channels     int    `json:"channels"`
	SampleRate   string `json:"sample_rate"`
	Tags         struct {
		Language string `json:"language"`
	} `json:"tags"`
}

type ffprobeFormat struct {
	FormatName string `json:"format_name"`
	BitRate    string `json:"bit_rate"`
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// runFFprobeOnSample feeds sample to ffprobe over stdin (no network access
// needed by ffprobe itself) and extracts the first video and audio stream's
// technical characteristics.
func runFFprobeOnSample(sample []byte) (*StreamTechInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), techProbeFFprobeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		"-analyzeduration", "3000000",
		"-probesize", strconv.Itoa(len(sample)),
		"-i", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(sample)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffprobe: %s", msg)
	}

	var out ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("ffprobe: failed to parse output: %w", err)
	}

	info := &StreamTechInfo{ContainerFormat: out.Format.FormatName}
	for _, s := range out.Streams {
		switch s.CodecType {
		case "video":
			if info.VideoCodec != "" {
				continue // keep the first video stream only
			}
			info.VideoCodec = s.CodecName
			info.Width = s.Width
			info.Height = s.Height
			info.FrameRate = parseFrameRate(s.AvgFrameRate)
			if info.FrameRate == 0 {
				info.FrameRate = parseFrameRate(s.RFrameRate)
			}
			info.VideoBitrateKbps = kbps(s.BitRate)
		case "audio":
			if info.AudioCodec != "" {
				continue // keep the first audio stream only
			}
			info.AudioCodec = s.CodecName
			info.AudioChannels = s.Channels
			if sr, err := strconv.Atoi(s.SampleRate); err == nil {
				info.AudioSampleRate = sr
			}
			info.AudioLanguage = s.Tags.Language
			info.AudioBitrateKbps = kbps(s.BitRate)
		}
	}

	if info.VideoCodec == "" && info.AudioCodec == "" {
		return nil, fmt.Errorf("ffprobe: no audio or video stream detected in sample")
	}
	return info, nil
}

// parseFrameRate converts an ffprobe "num/den" rational frame rate into a float.
func parseFrameRate(rate string) float64 {
	parts := strings.SplitN(rate, "/", 2)
	if len(parts) != 2 {
		return 0
	}
	num, errN := strconv.ParseFloat(parts[0], 64)
	den, errD := strconv.ParseFloat(parts[1], 64)
	if errN != nil || errD != nil || den == 0 {
		return 0
	}
	fps := num / den
	// Round to 2 decimals for a clean display value.
	return float64(int(fps*100+0.5)) / 100
}

// kbps converts an ffprobe bits-per-second string into whole kbps.
func kbps(bitRate string) int {
	bps, err := strconv.ParseInt(bitRate, 10, 64)
	if err != nil || bps <= 0 {
		return 0
	}
	return int(bps / 1000)
}
