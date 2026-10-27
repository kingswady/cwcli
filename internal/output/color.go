package output

import (
	"regexp"
	"strings"
)

const (
	Reset      = "\x1b[0m"
	Dim        = "\x1b[2m"
	Red        = "\x1b[31m"
	Green      = "\x1b[32m"
	Yellow     = "\x1b[33m"
	Cyan       = "\x1b[36m"
	reverse    = "\x1b[7m"
	reverseOff = "\x1b[27m" // ends the highlight without ending the colour around it
	Bold       = "\x1b[1m"
)

// odooLine is Odoo's log format: time, pid, level, database, logger: message.
var odooLine = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}) (\d+) (DEBUG|INFO|WARNING|ERROR|CRITICAL) (\S+) ([\w.\-]+): (.*)$`)

// httpStatus is the status in a werkzeug request line: `"GET / HTTP/1.1" 200 -`.
var httpStatus = regexp.MustCompile(`(" )([1-5]\d\d)( )`)

// Levels coloured the way Odoo's own terminal handler does.
var levelColor = map[string]string{
	"DEBUG":    "\x1b[1;34m",
	"INFO":     "\x1b[1;32m",
	"WARNING":  "\x1b[1;33m",
	"ERROR":    "\x1b[1;31m",
	"CRITICAL": "\x1b[1;37;41m",
}

// messageColor makes the message of a problem stand out; routine lines stay plain.
var messageColor = map[string]string{"WARNING": Yellow, "ERROR": Red, "CRITICAL": Red}

// LogLine is one log entry from the platform as cw prints it: Clean, then
// coloured for a terminal when color is on.
func LogLine(entry string, color bool, grep string) string {
	entry = Clean(entry)
	if !color {
		return entry
	}
	return Colorize(entry, grep)
}

// Colorize renders one log entry (it may span lines: a traceback) for a
// terminal. Every --grep match is found on the plain text first, so a match
// never lands inside a colour code, nor breaks where one begins.
func Colorize(entry, grep string) string {
	lines := strings.Split(entry, "\n")
	match := odooLine.FindStringSubmatch(lines[0])
	if match == nil {
		return highlight(entry, grep)
	}
	timestamp, pid, level, db, logger, message := match[1], match[2], match[3], match[4], match[5], match[6]
	tint := messageColor[level]
	var out strings.Builder
	out.WriteString(Dim + timestamp + " " + pid + Reset + " ")
	out.WriteString(levelColor[level] + level + Reset + " ")
	out.WriteString(Cyan + db + Reset + " " + Dim + logger + ":" + Reset + " ")
	out.WriteString(paintMessage(message, tint, logger == "werkzeug", grep))
	continuation := tint
	if continuation == "" {
		continuation = Dim
	}
	for _, line := range lines[1:] {
		out.WriteString("\n" + Paint(continuation, highlight(line, grep)))
	}
	return out.String()
}

func Paint(color, text string) string {
	if color == "" || text == "" {
		return text
	}
	return color + text + Reset
}

var statusColor = map[byte]string{'2': Green, '3': Cyan, '4': Yellow, '5': Red}

// paintMessage is message in tint, with a werkzeug request line's HTTP status
// in its class's colour — the tint resumes after it — and grep's matches marked.
func paintMessage(message, tint string, request bool, grep string) string {
	found := matches(message, grep)
	var out strings.Builder
	at := 0
	if request {
		for _, m := range httpStatus.FindAllStringSubmatchIndex(message, -1) {
			start, end := m[4], m[5] // the status digits
			out.WriteString(Paint(tint, marked(message, found, at, start)))
			out.WriteString(Paint(statusColor[message[start]], marked(message, found, start, end)))
			at = end
		}
	}
	out.WriteString(Paint(tint, marked(message, found, at, len(message))))
	return out.String()
}

// highlight marks every case-insensitive occurrence of grep in text.
func highlight(text, grep string) string {
	return marked(text, matches(text, grep), 0, len(text))
}

// matches is where grep occurs in text, any case, as [start, end) byte offsets.
func matches(text, grep string) [][2]int {
	if grep == "" {
		return nil
	}
	lower, needle := strings.ToLower(text), strings.ToLower(grep)
	if len(lower) != len(text) { // a case fold changed byte lengths: offsets would not line up
		return nil
	}
	var found [][2]int
	for at := 0; ; {
		i := strings.Index(lower[at:], needle)
		if i < 0 {
			return found
		}
		found = append(found, [2]int{at + i, at + i + len(needle)})
		at += i + len(needle)
	}
}

// marked is text[from:to] with the parts of found inside it in reverse video.
// found holds offsets in the whole text, so a match cut by a colour change is
// marked on both sides of it.
func marked(text string, found [][2]int, from, to int) string {
	var out strings.Builder
	at := from
	for _, m := range found {
		start, end := max(m[0], at), min(m[1], to)
		if start >= end {
			continue
		}
		out.WriteString(text[at:start] + reverse + text[start:end] + reverseOff)
		at = end
	}
	out.WriteString(text[at:to])
	return out.String()
}

// Column colours, keyed by the API field, in the dashboard's own language:
// production red, staging yellow, development cyan; deployed green, running
// cyan, draft grey, error red.
var cellStyles = map[string]func(string) string{
	"environment_type": func(v string) string {
		return map[string]string{"production": Red, "staging": Yellow, "development": Cyan}[v]
	},
	"state": stateColor,
	"backup_health": func(v string) string {
		return map[string]string{"healthy": Green, "warning": Yellow, "critical": Red, "none": Dim}[v]
	},
	"ssl_status": func(v string) string {
		return map[string]string{"valid": Green, "expired": Red, "error": Red, "expiring_soon": Yellow, "unknown": Yellow}[v]
	},
	"automated":      yesNoColor,
	"platform_login": yesNoColor,
	"primary":        yesNoColor,
	"main":           yesNoColor,
	"on_storage":     yesNoColor,
	"on_server":      yesNoColor,
	"maintenance": func(v string) string {
		return map[string]string{"on": Yellow}[v]
	},
	"verified": func(v string) string {
		return map[string]string{"yes": Green, "no": Yellow}[v]
	},
	"version": func(string) string { return Cyan },
	// A check's cell carries its reason: "failed (http_502)".
	"public_check": func(v string) string {
		switch {
		case v == "passed":
			return Green
		case strings.HasPrefix(v, "failed"):
			return Red
		}
		return Dim
	},
	"up_to_date": func(v string) string {
		return map[string]string{"up to date": Green, "behind": Yellow, "deleted upstream": Red, "unknown": Dim}[v]
	},
	// "1/2": every copy stored, or not yet.
	"offsite": func(v string) string {
		if stored, made, ok := strings.Cut(v, "/"); ok && stored == made {
			return Green
		}
		return Yellow
	},
	"outcome": func(v string) string {
		return map[string]string{"applied": Green, "running": Cyan, "failed": Red, "failed, kept as it was": Yellow}[v]
	},
	"backup_verified": func(v string) string {
		return map[string]string{"verified": Green, "not verified": Yellow}[v]
	},
}

func stateColor(state string) string {
	switch {
	case strings.HasSuffix(state, "-queue"):
		return Cyan
	case state == "deploy" || state == "done" || state == "successful":
		return Green
	case state == "running" || state == "partial" || state == "starting" || state == "started" || state == "queued":
		return Cyan
	case state == "retrying":
		return Yellow
	case state == "error" || state == "failed" || state == "timeout" || state == "interrupted" || state == "delete":
		return Red
	case state == "draft" || state == "idle" || state == "cancel" || state == "canceled" || state == "disabled":
		return Dim
	}
	return ""
}

func yesNoColor(v string) string {
	return map[string]string{"yes": Green, "no": Dim}[v]
}

// style colours one cell by its column; "-" (no value) stays plain.
func Style(key, text string) string {
	return StyleAs(key, text, text)
}

// StyleAs paints text in the colour key gives value: a sentence about a state,
// in that state's colour.
func StyleAs(key, value, text string) string {
	if fn := cellStyles[key]; fn != nil && value != "-" {
		return Paint(fn(value), text)
	}
	return text
}

func BoldAll(headers []string) []string {
	bold := make([]string, len(headers))
	for i, header := range headers {
		bold[i] = Paint(Bold, header)
	}
	return bold
}
