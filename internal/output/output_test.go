package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestColouredCellsStayAligned(t *testing.T) {
	var out bytes.Buffer
	rows := [][]string{{Paint(Green, "deploy"), "a"}, {"error", "b"}}
	if err := WriteTable(&out, BoldAll([]string{"STATE", "X"}), rows); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var columns []int
	for _, line := range lines {
		plain := ansiEscape.ReplaceAllString(line, "")
		columns = append(columns, len(plain)-1) // the second column's single letter ends each line
	}
	if columns[0] != columns[1] || columns[1] != columns[2] {
		t.Errorf("second column misaligned: %v\n%s", columns, out.String())
	}
}

func TestAgo(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = time.Now })
	cases := map[any]string{
		"2026-09-24 11:59:30":  "just now",
		"2026-09-24T11:15:00Z": "45m ago",
		"2026-09-24 11:15:00":  "45m ago",
		"2026-09-23 12:00:00":  "24h ago",
		"2026-09-20 12:00:00":  "4d ago",
		nil:                    "-",
	}
	for in, want := range cases {
		if got := Ago(in); got != want {
			t.Errorf("Ago(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanDropsControlCharactersButTabAndNewline(t *testing.T) {
	in := "a\x1b[31mb\tc\nd\re\x07f\u009bg\x7fh\x00i"
	if got, want := Clean(in), "a[31mb\tc\ndefghi"; got != want {
		t.Errorf("Clean(%q) = %q, want %q", in, got, want)
	}
	if got := Text("shop\x1b]0;owned\x07"); got != "shop]0;owned" {
		t.Errorf("Text keeps no control characters: %q", got)
	}
	if got := Text([]any{"x\x1by"}); strings.ContainsRune(got, 0x1b) {
		t.Errorf("nor for a value that is not a string: %q", got)
	}
}

func TestACellIsOneLine(t *testing.T) {
	var out bytes.Buffer
	if err := WriteTable(&out, []string{"NAME", "ID"}, [][]string{{"shop\n9999  forged", "7"}, {"a\tb", "8"}}); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 3 {
		t.Errorf("a value's newline forged a row:\n%s", out.String())
	}
	if strings.Contains(out.String(), "\t") {
		t.Errorf("a tab shifts the columns: %q", out.String())
	}
}

func TestJSONCarriesNoRawC1Characters(t *testing.T) {
	raw := json.RawMessage("{\"name\":\"a\u009b2Jb\x7f\"}")
	var out bytes.Buffer
	if err := WriteJSON(&out, raw); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(out.String(), 0x9b) || strings.ContainsRune(out.String(), 0x7f) {
		t.Errorf("raw control characters in %q", out.String())
	}
	var before, after map[string]string
	json.Unmarshal(raw, &before)
	if err := json.Unmarshal(out.Bytes(), &after); err != nil || after["name"] != before["name"] {
		t.Errorf("not the same JSON value: %q (%v)", out.String(), err)
	}
}

func TestExpiresSaysHowLongIsLeftOrHowLongAgo(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = time.Now })
	cases := map[any]string{
		"2026-11-26T22:27:57Z": "in 41 days",
		"2026-10-16T18:00:00Z": "in 6 hours",
		"2026-10-13T12:00:00Z": "3d ago",
		nil:                    "-",
	}
	for in, want := range cases {
		if got := Expires(in); got != want {
			t.Errorf("Expires(%v) = %q, want %q", in, got, want)
		}
	}
}
