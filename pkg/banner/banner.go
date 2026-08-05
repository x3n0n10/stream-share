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

package banner

import (
	"fmt"
	"os"

	isatty "github.com/mattn/go-isatty"
)

// green/brightGreen match the StreamShare logo's converging-streams (#65c86b)
// and play-signal (#9bf89f) colors.
const (
	green       = "\x1b[38;2;101;200;107m"
	brightGreen = "\x1b[38;2;155;248;159m"
	bold        = "\x1b[1m"
	reset       = "\x1b[0m"
)

// streams is the plain-text rendering of the three converging streams.
// triangle is the play signal they feed into, sized to match the streams'
// full height rather than a single-character glyph.
var (
	streams = [5]string{
		`      \ `,
		`       \`,
		`--------`,
		`       /`,
		`      / `,
	}
	triangle = [5]string{
		`   |\`,
		`   | \`,
		`   |   >`,
		`   | /`,
		`   |/`,
	}
)

// Print writes an ASCII rendering of the StreamShare logo to stdout, colored
// when stdout is a terminal and plain otherwise (e.g. when piped to a log
// collector).
func Print() {
	color := isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())

	for i := range streams {
		if !color {
			line := streams[i] + triangle[i]
			if i == 2 {
				line += "  stream-share"
			}
			fmt.Println(line)
			continue
		}

		line := green + streams[i] + reset + brightGreen + triangle[i] + reset
		if i == 2 {
			line += "  " + bold + "stream-share" + reset
		}
		fmt.Println(line)
	}
}
