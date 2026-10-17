// Package output is how cw prints: aligned tables, colours, JSON and times.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// legacyTime is how API servers before RFC 3339 datetimes wrote them (naive UTC).
const legacyTime = "2006-01-02 15:04:05"

// now is swapped in tests.
var now = time.Now

// Clean is text from the platform made safe for a terminal: control
// characters (C0, DEL, C1: ESC, CSI, BEL, a carriage return that overwrites
// the line) are dropped, all but tab and newline. It is the one place this
// happens — everything the platform says passes through it (Text, LogLine,
// error messages) before cw adds colours of its own.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return -1
		}
		return r
	}, s)
}

// WriteJSON indents the API's JSON. JSON has no raw C0 characters, but it
// may carry DEL and C1 ones raw (U+009B is a CSI to some terminals): they
// are written as \u escapes, the same JSON value.
func WriteJSON(w io.Writer, raw json.RawMessage) error {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return err
	}
	var safe strings.Builder
	for _, r := range out.String() {
		if unicode.IsControl(r) && r >= 0x7f {
			fmt.Fprintf(&safe, "\\u%04x", r)
			continue
		}
		safe.WriteRune(r)
	}
	safe.WriteByte('\n')
	_, err := io.WriteString(w, safe.String())
	return err
}

// ansiEscape matches the colour codes this program writes.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

// visibleWidth is what a terminal shows of s: colour codes take no room.
func visibleWidth(s string) int {
	return utf8.RuneCountInString(ansiEscape.ReplaceAllString(s, ""))
}

var oneLine = strings.NewReplacer("\n", " ", "\t", " ")

// writeTable aligns rows under headers by visible width (text/tabwriter would
// count colour codes as characters); nil headers print rows only.
func WriteTable(w io.Writer, headers []string, rows [][]string) error {
	all := make([][]string, 0, len(rows)+1)
	if headers != nil {
		all = append(all, headers)
	}
	for _, row := range rows {
		// One line per row: a newline or tab inside a value would forge a row or shift a column.
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = oneLine.Replace(cell)
		}
		all = append(all, cells)
	}
	var widths []int
	for _, row := range all {
		for i, cell := range row {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], visibleWidth(cell))
		}
	}
	for _, row := range all {
		var line strings.Builder
		for i, cell := range row {
			line.WriteString(cell)
			if i < len(row)-1 {
				line.WriteString(strings.Repeat(" ", widths[i]-visibleWidth(cell)+2))
			}
		}
		if _, err := fmt.Fprintln(w, strings.TrimRight(line.String(), " ")); err != nil {
			return err
		}
	}
	return nil
}

// Text renders one JSON value for a cell, Clean: null and "" read as "-".
func Text(value any) string {
	switch v := value.(type) {
	case nil:
		return "-"
	case string:
		if v == "" {
			return "-"
		}
		return Clean(v)
	case bool:
		if v {
			return "yes"
		}
		return "no"
	case json.Number:
		return v.String()
	default:
		return Clean(fmt.Sprint(v))
	}
}

func ParseTime(value any) (time.Time, bool) {
	s, ok := value.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	t, err := time.ParseInLocation(legacyTime, s, time.UTC)
	return t, err == nil
}

// LocalTime is the absolute time in this machine's zone.
func LocalTime(value any) string {
	t, ok := ParseTime(value)
	if !ok {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// Ago is a short relative time for table cells.
func Ago(value any) string {
	t, ok := ParseTime(value)
	if !ok {
		return "-"
	}
	d := now().Sub(t)
	switch {
	case d < 0:
		return t.Local().Format("2006-01-02")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

// Expires is when something ends: "in 41 days", or how long ago it did.
func Expires(value any) string {
	t, ok := ParseTime(value)
	if !ok {
		return "-"
	}
	if current := now(); t.After(current) {
		return Until(current, t)
	}
	return Ago(value)
}

func Megabytes(value any) string {
	n, ok := value.(json.Number)
	if !ok {
		return "-"
	}
	mb, err := n.Float64()
	if err != nil {
		return "-"
	}
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", mb/1024)
	}
	return fmt.Sprintf("%.1f MB", mb)
}
