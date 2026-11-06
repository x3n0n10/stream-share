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

package xtream

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// ActionToFile is ActionContext that writes the 200 body to path instead of
// decoding it: path+".part" is renamed to path on success and removed on any
// failure. Failures and empty bodies answer http.StatusBadGateway like
// ActionContext.
func (c *Client) ActionToFile(ctx context.Context, cfg *config.ProxyConfig, action string, q url.Values, path string) (httpcode int, err error) {
	utils.DebugLog("Processing Xtream action=%s (to file)", action)
	rawURL, err := c.requestURL(action, q)
	if err != nil {
		return http.StatusInternalServerError, utils.PrintErrorAndReturn(err)
	}

	part := path + ".part"
	var size int64
	ok, lastErr := c.fetchWithRetry(ctx, rawURL, action, func(resp *http.Response, cancel context.CancelFunc) (int64, error) {
		f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return 0, err
		}
		stall := time.AfterFunc(bodyStallTimeout, cancel)
		defer stall.Stop()
		size, err = io.Copy(f, &stallReader{r: io.LimitReader(resp.Body, maxResponseBytes+1), timer: stall})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return size, err
	})
	if ok && size > 0 {
		if err := os.Rename(part, path); err != nil {
			_ = os.Remove(part)
			return http.StatusBadGateway, err
		}
		return http.StatusOK, nil
	}
	_ = os.Remove(part)
	utils.DebugLog("Request failed, last error: %v", lastErr)
	return http.StatusBadGateway, lastErr
}

// CatalogFileKind says how a classified catalogue file is to be used.
type CatalogFileKind int

const (
	CatalogFileArray CatalogFileKind = iota // non-empty JSON array; stays on disk
	CatalogFileSmall                        // decoded value in small (object, [], {}, fallback)
)

// ClassifyCatalogFile reproduces the outcomes of ActionContext's body handling
// on a file. A top-level array is only validated, element by element, so
// memory does not grow with its size.
func ClassifyCatalogFile(path, action string) (kind CatalogFileKind, small interface{}, err error) {
	f, err := os.Open(path)
	if err != nil {
		return CatalogFileSmall, FallbackForAction(action), err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64<<10)
	var first byte
	for {
		first, err = r.ReadByte()
		if err == io.EOF {
			return CatalogFileSmall, FallbackForAction(action), nil
		}
		if err != nil {
			return CatalogFileSmall, FallbackForAction(action), err
		}
		if strings.IndexByte(" \t\r\n", first) < 0 {
			break
		}
	}
	_ = r.UnreadByte()

	dec := json.NewDecoder(r)
	dec.UseNumber()
	switch first {
	case '<':
		return CatalogFileSmall, FallbackForAction(action), nil
	case '[':
		n, err := validateArray(dec)
		if err != nil {
			utils.DebugLog("JSON decoding failed: %v", err)
			return CatalogFileSmall, FallbackForAction(action), err
		}
		if n == 0 {
			return CatalogFileSmall, []interface{}{}, nil
		}
		return CatalogFileArray, nil, nil
	}
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		utils.DebugLog("JSON decoding failed: %v", err)
		return CatalogFileSmall, FallbackForAction(action), err
	}
	if v == nil { // literal null
		return CatalogFileSmall, FallbackForAction(action), nil
	}
	return CatalogFileSmall, v, nil
}

// validateArray consumes one whole array and the end of input, returning the
// element count.
func validateArray(dec *json.Decoder) (int, error) {
	if _, err := dec.Token(); err != nil {
		return 0, err
	}
	n := 0
	for dec.More() {
		var el json.RawMessage
		if err := dec.Decode(&el); err != nil {
			return 0, err
		}
		n++
	}
	if _, err := dec.Token(); err != nil {
		return 0, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	return n, nil
}
