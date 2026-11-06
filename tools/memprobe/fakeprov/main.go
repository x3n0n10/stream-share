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

// Fake Xtream provider for tools/memprobe/measure.sh.
package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

func list(kind string, n int) []byte {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		switch kind {
		case "live":
			fmt.Fprintf(&b, `{"num":%d,"name":"NL| Channel %d HD","stream_type":"live","stream_id":%d,"stream_icon":"http://img.example.com/logos/%d.png","epg_channel_id":"ch%d.nl","added":"1700000000","category_id":"%d","category_ids":[%d],"custom_sid":"","tv_archive":%d,"direct_source":"","tv_archive_duration":3}`, i+1, i, i+1, i, i, i%300+1, i%300+1, i%2)
		case "vod":
			fmt.Fprintf(&b, `{"num":%d,"name":"Movie Title Number %d (2021)","stream_type":"movie","stream_id":%d,"stream_icon":"http://img.example.com/posters/%d.jpg","rating":"6.5","rating_5based":3.2,"added":"1700000000","category_id":"%d","category_ids":[%d],"container_extension":"mkv","custom_sid":"","direct_source":""}`, i+1, i, 1000000+i, i, i%400+1, i%400+1)
		case "series":
			fmt.Fprintf(&b, `{"num":%d,"name":"Series Name %d","series_id":%d,"cover":"http://img.example.com/covers/%d.jpg","plot":"A long plot description for series %d that goes on for a while to be realistic about the size of provider answers.","cast":"Actor One, Actor Two, Actor Three","director":"Some Director","genre":"Drama / Thriller","releaseDate":"2019-01-01","last_modified":"1700000000","rating":"7","rating_5based":3.5,"backdrop_path":["http://img.example.com/bd/%d.jpg"],"youtube_trailer":"","episode_run_time":"45","category_id":"%d","category_ids":[%d]}`, i+1, i, i+1, i, i, i, i%200+1, i%200+1)
		}
	}
	b.WriteString("]")
	return []byte(b.String())
}

func cats(n int) []byte {
	var b strings.Builder
	b.WriteString("[")
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"category_id":"%d","category_name":"Category %d","parent_id":0}`, i, i)
	}
	b.WriteString("]")
	return []byte(b.String())
}

func main() {
	n := func(env string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(env)); err == nil {
			return v
		}
		return def
	}
	nl, nv, ns := n("NLIVE", 22000), n("NVOD", 122000), n("NSERIES", 20000)
	bodies := map[string][]byte{
		"get_live_streams": list("live", nl), "get_vod_streams": list("vod", nv), "get_series": list("series", ns),
		"get_live_categories": cats(300), "get_vod_categories": cats(400), "get_series_categories": cats(200),
	}
	base := map[string][]byte{}
	for k, v := range bodies {
		base[k] = v
	}
	var mu sync.RWMutex
	var m3u strings.Builder
	m3u.WriteString("#EXTM3U\n")
	for i := 0; i < nl; i++ {
		fmt.Fprintf(&m3u, "#EXTINF:-1 tvg-id=\"ch%d.nl\" tvg-name=\"NL| Channel %d HD\" tvg-logo=\"http://img.example.com/logos/%d.png\" group-title=\"Category %d\",NL| Channel %d HD\nHOST/live/u/p/%d.ts\n", i, i, i, i%300+1, i, i+1)
	}
	for i := 0; i < nv; i++ {
		fmt.Fprintf(&m3u, "#EXTINF:-1 tvg-id=\"\" tvg-name=\"Movie Title Number %d (2021)\" tvg-logo=\"http://img.example.com/posters/%d.jpg\" group-title=\"Category %d\",Movie Title Number %d (2021)\nHOST/movie/u/p/%d.mkv\n", i, i, i%400+1, i, 1000000+i)
	}
	m3uBody := m3u.String()
	addr := os.Getenv("ADDR")
	for k, v := range bodies {
		fmt.Printf("%s %.1f MB\n", k, float64(len(v))/(1<<20))
	}
	fmt.Printf("m3u %.1f MB\n", float64(len(m3uBody))/(1<<20))
	http.HandleFunc("/player_api.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		a := r.URL.Query().Get("action")
		if a == "" {
			_, _ = fmt.Fprintf(w, `{"user_info":{"username":"u","password":"p","auth":1,"status":"Active","exp_date":"1900000000","is_trial":"0","active_cons":"0","created_at":"1600000000","max_connections":"1","allowed_output_formats":["m3u8","ts"]},"server_info":{"url":"127.0.0.1","port":"%s","https_port":"","server_protocol":"http","rtmp_port":"","timezone":"UTC","timestamp_now":1700000000,"time_now":"2023-11-14 00:00:00"}}`, strings.TrimPrefix(addr, ":"))
			return
		}
		mu.RLock()
		b, ok := bodies[a]
		mu.RUnlock()
		if ok {
			_, _ = w.Write(b)
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	// Renames the first item of each list so a forced refresh gets a new body.
	bumps := 0
	http.HandleFunc("/bump", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		bumps++
		for _, k := range []string{"get_live_streams", "get_vod_streams", "get_series"} {
			bodies[k] = bytes.Replace(base[k], []byte(`"name":"`), []byte(fmt.Sprintf(`"name":"B%d `, bumps)), 1)
		}
		_, _ = fmt.Fprintf(w, "bump %d\n", bumps)
	})
	http.HandleFunc("/get.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/x-mpegurl")
		_, _ = w.Write([]byte(strings.ReplaceAll(m3uBody, "HOST", "http://127.0.0.1"+addr)))
	})
	http.HandleFunc("/xmltv.php", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><tv></tv>`))
	})
	fmt.Println("listening", addr)
	panic(http.ListenAndServe(addr, nil))
}
