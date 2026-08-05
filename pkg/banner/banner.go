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

// art is the plain-text (no color) rendering of the logo: three streams
// converging into a single play signal.
var art = [5]string{
	`      \`,
	`       \`,
	`--------o>`,
	`       /`,
	`      /`,
}

// Print writes an ASCII rendering of the StreamShare logo to stdout, colored
// when stdout is a terminal and plain otherwise (e.g. when piped to a log
// collector).
func Print() {
	if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
		fmt.Println(green + art[0] + reset)
		fmt.Println(green + art[1] + reset)
		fmt.Println(green + art[2][:8] + brightGreen + art[2][8:] + reset + "  " + bold + "stream-share" + reset)
		fmt.Println(green + art[3] + reset)
		fmt.Println(green + art[4] + reset)
		return
	}

	for i, line := range art {
		if i == 2 {
			fmt.Println(line + "  stream-share")
			continue
		}
		fmt.Println(line)
	}
}
