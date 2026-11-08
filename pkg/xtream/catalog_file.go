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

package xtream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// errLocalWrite marks a failure on our own disk: fetchWithRetry must not
// re-download the body from the provider for it.
var errLocalWrite = errors.New("local catalogue write")

// localWriter tags write errors so they can be told from body read errors.
type localWriter struct{ w io.Writer }

func (l localWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if err != nil {
		err = fmt.Errorf("%w: %v", errLocalWrite, err)
	}
	return n, err
}

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
			return 0, fmt.Errorf("%w: %v", errLocalWrite, err)
		}
		stall := time.AfterFunc(bodyStallTimeout, cancel)
		defer stall.Stop()
		size, err = io.Copy(localWriter{f}, &stallReader{r: io.LimitReader(resp.Body, maxResponseBytes+1), timer: stall})
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("%w: %v", errLocalWrite, cerr)
		}
		return size, err
	})
	if ok && size > 0 {
		if err := os.Rename(part, path); err != nil {
			_ = os.Remove(part)
			return http.StatusBadGateway, fmt.Errorf("%w: %v", errLocalWrite, err)
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
	defer func() { _ = f.Close() }()

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

// validateArray consumes one whole array, returning the element count.
// Anything after the closing bracket is ignored, as ActionContext's single
// Decode does. A truncated array is io.ErrUnexpectedEOF, as Decode reports it.
func validateArray(dec *json.Decoder) (int, error) {
	n := 0
	_, err := dec.Token()
	for err == nil && dec.More() {
		var el json.RawMessage
		err = dec.Decode(&el)
		n++
	}
	if err == nil {
		_, err = dec.Token()
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
