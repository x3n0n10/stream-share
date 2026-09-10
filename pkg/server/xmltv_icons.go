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
	"encoding/xml"
	"io"

	"github.com/gin-gonic/gin"
)

// rewriteXMLTVIcons rewrites the src attribute of every <icon> element in
// an XMLTV document — legal on both <channel> and <programme> per the
// XMLTV DTD, so no distinction is made between them — to point through
// this proxy instead of the upstream provider. Implemented as a token-
// stream copy so entity-escaped characters and untouched document
// structure round-trip correctly; <url> elements are a separate tag and
// are left untouched. Falls back to the original bytes unchanged if the
// document fails to decode, so a non-XML upstream error body doesn't
// error the whole EPG response.
func (c *Config) rewriteXMLTVIcons(ctx *gin.Context, body []byte) []byte {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out bytes.Buffer
	enc := xml.NewEncoder(&out)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return body
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "icon" {
			for i, attr := range se.Attr {
				if attr.Name.Local == "src" {
					se.Attr[i].Value = c.proxyImageURL(ctx, attr.Value)
				}
			}
			tok = se
		}
		if err := enc.EncodeToken(tok); err != nil {
			return body
		}
	}
	if err := enc.Flush(); err != nil {
		return body
	}
	return out.Bytes()
}
