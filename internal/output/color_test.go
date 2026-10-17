package output

import (
	"strings"
	"testing"
)

const infoLine = "2026-09-26 13:16:25,466 56 INFO v19-0 odoo.addons.base.models.ir_cron: Job done"

func TestAnInfoLineGetsOdoosColours(t *testing.T) {
	got := Colorize(infoLine, "")
	for _, want := range []string{
		Dim + "2026-09-26 13:16:25,466 56" + Reset,
		"\x1b[1;32mINFO" + Reset,
		Cyan + "v19-0" + Reset,
		Dim + "odoo.addons.base.models.ir_cron:" + Reset,
		" Job done",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, Red) {
		t.Errorf("a routine line must not look like an error: %q", got)
	}
}

func TestAnErrorAndItsTracebackAreRed(t *testing.T) {
	entry := "2026-09-26 13:16:25,466 56 ERROR v19-0 odoo.http: Exception during request\nTraceback (most recent call last):\n  File \"x.py\", line 1"
	lines := strings.Split(Colorize(entry, ""), "\n")
	if !strings.Contains(lines[0], "\x1b[1;31mERROR") || !strings.Contains(lines[0], Red+"Exception during request") {
		t.Errorf("first line: %q", lines[0])
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, Red) {
			t.Errorf("traceback line not red: %q", line)
		}
	}
}

func TestHTTPStatusIsColouredByClass(t *testing.T) {
	for status, color := range map[string]string{"200": Green, "302": Cyan, "404": Yellow, "500": Red} {
		line := `2026-09-26 13:16:25,466 56 INFO v19-0 werkzeug: 1.2.3.4 - - [26/Sep/2026] "GET / HTTP/1.1" ` + status + " - 3"
		if got := Colorize(line, ""); !strings.Contains(got, color+status+Reset) {
			t.Errorf("%s: %q", status, got)
		}
	}
}

func TestGrepMatchesAreHighlightedInTheirOwnCase(t *testing.T) {
	got := Colorize(infoLine, "job")
	if !strings.Contains(got, reverse+"Job"+reverseOff) {
		t.Errorf("%q", got)
	}
}

func TestLinesThatAreNotOdoosAreLeftAlone(t *testing.T) {
	for _, line := range []string{"plain text", "Traceback (most recent call last):"} {
		if got := Colorize(line, ""); got != line {
			t.Errorf("%q became %q", line, got)
		}
	}
}

func TestSSLStatusIsColoured(t *testing.T) {
	if got := Style("ssl_status", "expired"); got != Red+"expired"+Reset {
		t.Errorf("got %q", got)
	}
}

func TestALogLineIsCleanedBeforeItIsColoured(t *testing.T) {
	entry := "2026-09-26 13:16:25,466 56 ERROR v19-0 odoo.http: boom\x1b[2J\x1b]0;owned\x07\nTraceback\r"
	got := LogLine(entry, true, "")
	for _, evil := range []string{"\x1b[2J", "\x1b]0", "\x07", "\r"} {
		if strings.Contains(got, evil) {
			t.Errorf("%q survived: %q", evil, got)
		}
	}
	if !strings.Contains(got, "\x1b[1;31mERROR") {
		t.Errorf("cw's own colours come after: %q", got)
	}
	if plain := LogLine(entry, false, ""); strings.ContainsRune(plain, 0x1b) || !strings.Contains(plain, "boom[2J]0;owned\nTraceback") {
		t.Errorf("plain: %q", plain)
	}
}

const requestLine = `2026-09-26 13:16:25,466 56 INFO v19-0 werkzeug: 1.2.3.4 - - [26/Sep/2026] "GET / HTTP/1.1" 200 - 3`

func TestGrepNeverMatchesInsideAColourCode(t *testing.T) {
	// "32" is not in the line — only in the green of its status, \x1b[32m.
	if got, want := Colorize(requestLine, "32"), Colorize(requestLine, ""); got != want {
		t.Errorf("a match inside an escape code:\n got %q\nwant %q", got, want)
	}
}

func TestGrepMatchesAcrossTheColouredStatus(t *testing.T) {
	got := Colorize(requestLine, `1.1" 200 -`)
	if plain := ansiEscape.ReplaceAllString(got, ""); plain != requestLine {
		t.Errorf("the text changed:\n%q\n%q", plain, requestLine)
	}
	for _, want := range []string{reverse + `1.1" ` + reverseOff, Green + reverse + "200" + reverseOff + Reset, reverse + " -" + reverseOff} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestTheMessageTintResumesAfterTheStatus(t *testing.T) {
	line := strings.Replace(requestLine, " INFO ", " WARNING ", 1)
	if got := Colorize(line, ""); !strings.Contains(got, Green+"200"+Reset+Yellow+" - 3"+Reset) {
		t.Errorf("the rest of a warning is still yellow: %q", got)
	}
}

func TestNewColumnsAreColouredByTheirMeaning(t *testing.T) {
	cases := []struct{ key, text, color string }{
		{"state", "retrying", Yellow},
		{"public_check", "passed", Green},
		{"public_check", "failed (http_502)", Red},
		{"public_check", "not checked (app_stopped)", Dim},
		{"up_to_date", "up to date", Green},
		{"up_to_date", "behind", Yellow},
		{"up_to_date", "deleted upstream", Red},
		{"offsite", "2/2", Green},
		{"offsite", "1/2", Yellow},
		{"outcome", "applied", Green},
		{"outcome", "failed", Red},
		{"outcome", "failed, kept as it was", Yellow},
		{"ssl_status", "valid", Green},
		{"verified", "no", Yellow},
		{"primary", "yes", Green},
	}
	for _, c := range cases {
		if got := Style(c.key, c.text); got != c.color+c.text+Reset {
			t.Errorf("%s %q: %q", c.key, c.text, got)
		}
	}
}
