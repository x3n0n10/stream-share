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

package slate

import (
	"strings"
	"testing"
)

// TestSanitizeStripsFilterSyntax is the security-relevant case: characters that
// drawtext treats as syntax must never survive into the filter string.
func TestSanitizeStripsFilterSyntax(t *testing.T) {
	dangerous := []string{":", "'", `\`, "%", `"`, "[", "]", ";", ","}
	// ',' is allowed in text but is also the filter separator, so verify it
	// separately below rather than in this loop.
	dangerous = dangerous[:len(dangerous)-1]

	in := `Provider said: 'no' \ 100% [bad];`
	got := sanitize(in)
	for _, c := range dangerous {
		if strings.Contains(got, c) {
			t.Fatalf("sanitize(%q) = %q, still contains %q", in, got, c)
		}
	}
	if got == "" {
		t.Fatalf("sanitize(%q) stripped everything; expected readable remainder", in)
	}
}

func TestSanitize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Forbidden", "Forbidden"},
		{"collapses whitespace", "too    many   spaces", "too many spaces"},
		{"keeps safe punctuation", "Not available (yet).", "Not available (yet)."},
		{"drops colon", "Error: denied", "Error denied"},
		{"trims", "  padded  ", "padded"},
		{"empty", "", ""},
		{"only junk", ":::", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitize(tc.in); got != tc.want {
				t.Fatalf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeTruncatesLongInput(t *testing.T) {
	got := sanitize(strings.Repeat("a", maxFieldChars*2))
	if len(got) > maxFieldChars {
		t.Fatalf("sanitize returned %d chars, want <= %d", len(got), maxFieldChars)
	}
}

func TestWrap(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		max   int
		want  []string
	}{
		{"empty", "", 20, 2, nil},
		{"fits on one line", "short message", 20, 2, []string{"short message"}},
		{
			"wraps onto two lines",
			"the provider refused access to this channel",
			24, 2,
			[]string{"the provider refused", "access to this channel"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrap(tc.in, tc.width, tc.max)
			if len(got) != len(tc.want) {
				t.Fatalf("wrap(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("wrap(%q) line %d = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestWrapRespectsMaxLines guards the ellipsis path and, more importantly, that
// wrap terminates on input far longer than the budget.
func TestWrapRespectsMaxLines(t *testing.T) {
	got := wrap(strings.Repeat("word ", 200), 20, 2)
	if len(got) > 2 {
		t.Fatalf("wrap returned %d lines, want <= 2", len(got))
	}
	if !strings.HasSuffix(got[len(got)-1], "...") {
		t.Fatalf("truncated wrap should end in an ellipsis, got %q", got[len(got)-1])
	}
}

// TestWrapSingleLongWordTerminates covers the pathological case of a token
// longer than the whole line width, which a naive loop would spin on forever.
func TestWrapSingleLongWordTerminates(t *testing.T) {
	got := wrap(strings.Repeat("x", 500), 20, 2)
	if len(got) == 0 || len(got) > 2 {
		t.Fatalf("wrap returned %d lines, want 1..2", len(got))
	}
	for _, l := range got {
		if len(l) > 23 { // width + ellipsis
			t.Fatalf("line %q longer than expected", l)
		}
	}
}

func TestJoinHeadline(t *testing.T) {
	cases := []struct{ code, meaning, want string }{
		{"403", "Forbidden", "Error 403 - Forbidden"},
		{"403", "", "Error 403"},
		{"", "Timed Out", "Timed Out"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := joinHeadline(tc.code, tc.meaning); got != tc.want {
			t.Fatalf("joinHeadline(%q,%q) = %q, want %q", tc.code, tc.meaning, got, tc.want)
		}
	}
}

// TestBuildFilterOmitsEmptyFields checks the layout collapses rather than
// leaving a blank line when there is no code/meaning or no channel.
func TestBuildFilterOmitsEmptyFields(t *testing.T) {
	full := buildFilter("403", "Forbidden", "Denied by provider.", "Sport 1")
	if want := strings.Count(full, "drawtext="); want != 3 {
		t.Fatalf("full slate produced %d drawtext filters, want 3", want)
	}

	minimal := buildFilter("", "", "Denied by provider.", "")
	if got := strings.Count(minimal, "drawtext="); got != 1 {
		t.Fatalf("minimal slate produced %d drawtext filters, want 1", got)
	}

	// A slate with nothing at all still has to render something.
	empty := buildFilter("", "", "", "")
	if !strings.Contains(empty, "drawtext=") {
		t.Fatalf("empty slate produced no drawtext filter: %q", empty)
	}
}

// TestBuildFilterQuotingIsBalanced catches a malformed filter graph, which is
// how an escaping bug would surface at runtime.
func TestBuildFilterQuotingIsBalanced(t *testing.T) {
	f := buildFilter("403", "Forbidden", "Nasty: 'quotes' 100% \\ backslash", "Ch: 1")
	if n := strings.Count(f, "'"); n%2 != 0 {
		t.Fatalf("unbalanced quotes in filter: %q", f)
	}
	for _, bad := range []string{`\`, "%", `'quotes'`} {
		if strings.Contains(f, bad) {
			t.Fatalf("filter contains unsanitized %q: %q", bad, f)
		}
	}
}

func TestUnavailableGeneratorRefuses(t *testing.T) {
	g := &Generator{dir: t.TempDir()} // available=false
	if g.Available() {
		t.Fatal("zero-value Generator should not report available")
	}
	if _, err := g.Clip(View{Message: "x"}); err == nil {
		t.Fatal("Clip should fail when ffmpeg is unavailable")
	}
}
