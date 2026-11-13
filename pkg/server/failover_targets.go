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
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/utils"
)

type failoverInstance struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Key  string `json:"key"`
}

// failoverPeer is a fallback with its base URL and header prepared once, so
// failoverTargets does no parsing or I/O.
type failoverPeer struct {
	name   string
	base   *url.URL
	header http.Header
}

// parseFailoverInstances reads the ordered fallback list; one bad entry makes
// the whole value invalid so a typo never silently drops a fallback.
func parseFailoverInstances(raw string) ([]failoverInstance, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var list []failoverInstance
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	for _, in := range list {
		u, err := url.Parse(in.URL)
		if in.Name == "" || in.URL == "" || in.Key == "" || err != nil ||
			(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("invalid failover instance")
		}
	}
	return list, nil
}

// setupFailover gives the session manager its fallbacks. Logs carry names
// only: the value, its parse error, the URLs and the keys never appear.
func (c *Config) setupFailover() {
	list, err := parseFailoverInstances(c.FailoverInstances)
	if err != nil {
		utils.WarnLog("Failover: FAILOVER_INSTANCES is not valid; failover is off")
		return
	}
	if len(list) == 0 {
		return
	}
	names := make([]string, 0, len(list))
	for _, in := range list {
		utils.RegisterSecrets(in.Key)
		base, _ := url.Parse(in.URL) // validated above
		h := http.Header{}
		h.Set(failoverKeyHeader, in.Key)
		h.Set(failoverFromHeader, c.InstanceName)
		c.failoverPeers = append(c.failoverPeers, failoverPeer{in.Name, base, h})
		names = append(names, in.Name)
	}
	utils.InfoLog("Failover: fallback instances in order: %s", strings.Join(names, ", "))
	c.sessionManager.SetFailoverTargets(c.failoverTargets)
}

// failoverTargets is the session manager's targets function for a stream key.
func (c *Config) failoverTargets(streamID string) []session.FailoverTarget {
	id := normalizeStreamID(streamID)
	epg, _ := lookupEPGChannelID(id)
	name, _ := lookupAPIChannelName(id)
	if epg == "" && name == "" {
		return nil
	}
	q := url.Values{"epg": {epg}, "name": {name}}.Encode()
	out := make([]session.FailoverTarget, 0, len(c.failoverPeers))
	for _, p := range c.failoverPeers {
		u := *p.base
		u.Path = strings.TrimRight(u.Path, "/") + "/api/failover/live"
		u.RawPath = ""
		u.RawQuery = q
		out = append(out, session.FailoverTarget{Name: p.name, URL: &u, Header: p.header})
	}
	return out
}
