/*
 * stream-share is a project to efficiently share the use of an IPTV service.
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
	"bytes"
	"encoding/xml"
	"io"

	"github.com/gin-gonic/gin"
)

// rewriteXMLTVIconsTo copies an XMLTV document from r to w, rewriting the src
// attribute of every <icon> element — legal on both <channel> and <programme>
// per the XMLTV DTD, so no distinction is made between them — to point through
// this proxy instead of the upstream provider. Implemented as a token-stream
// copy, so memory stays at one element whatever the guide's size, and entity-
// escaped characters and untouched document structure round-trip correctly;
// <url> elements are a separate tag and are left untouched.
func (c *Config) rewriteXMLTVIconsTo(ctx *gin.Context, r io.Reader, w io.Writer) error {
	dec := xml.NewDecoder(r)
	enc := xml.NewEncoder(w)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
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
			return err
		}
	}
	return enc.Flush()
}

// rewriteXMLTVIcons is the in-memory form of rewriteXMLTVIconsTo. It falls back
// to the original bytes unchanged if the document fails to decode, so a
// non-XML upstream error body doesn't error the whole EPG response.
func (c *Config) rewriteXMLTVIcons(ctx *gin.Context, body []byte) []byte {
	var out bytes.Buffer
	if err := c.rewriteXMLTVIconsTo(ctx, bytes.NewReader(body), &out); err != nil {
		return body
	}
	return out.Bytes()
}
