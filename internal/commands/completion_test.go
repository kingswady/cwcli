package commands

import (
	"strings"
	"testing"
)

func complete(t *testing.T, words ...string) []string {
	t.Helper()
	h := newHarness(t, nil)
	if code := h.run(append([]string{completeCommand}, words...)...); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if h.stderr.Len() != 0 {
		t.Errorf("a Tab writes nothing on stderr: %q", h.stderr)
	}
	return strings.Fields(h.stdout.String())
}

func TestTabCompletesCommandsFlagsAndValues(t *testing.T) {
	cases := []struct {
		words []string
		want  string
	}{
		{[]string{""}, "restore-points"},
		{[]string{"re"}, "restore-points"},
		{[]string{"apps", ""}, "show"},
		{[]string{"apps", "--sh"}, "--show-url"},
		{[]string{"apps", "--env", ""}, "staging"},
		{[]string{"apps", "--env=st"}, "--env=staging"},
		{[]string{"apps", "--env", "=", ""}, "production"}, // bash splits at "="
		{[]string{"logs", "shop", "--source", "up"}, "upgrade"},
		{[]string{"urls", "--ssl", "exp"}, "expiring_soon"},
		{[]string{"restore-points", "shop", "--pro"}, "--project"},
		{[]string{"completion", ""}, "zsh"},
		{[]string{"attention", "--"}, "--json"},
	}
	for _, c := range cases {
		got := complete(t, c.words...)
		if !contains(got, c.want) {
			t.Errorf("%q: %q has no %q", c.words, got, c.want)
		}
	}
}

func TestTabOffersOnlyWhatFits(t *testing.T) {
	cases := []struct {
		words []string
		want  string
	}{
		{[]string{"apps", "show", ""}, ""},                                     // the name is the platform's: nothing offline
		{[]string{"urls", ""}, ""},                                             // urls has no show
		{[]string{"apps", "--json", "sh"}, "show"},                             // --json takes no value
		{[]string{"apps", "--project", "x", ""}, "show update update-modules"}, // --project does
		{[]string{"nope", "--"}, ""},
	}
	for _, c := range cases {
		if got := strings.Join(complete(t, c.words...), " "); got != c.want {
			t.Errorf("%q: %q, want %q", c.words, got, c.want)
		}
	}
}

func TestEveryCommandsFlagsAreFound(t *testing.T) {
	h := newHarness(t, nil)
	for _, command := range commandNames() {
		if command == "help" || command == "version" {
			continue
		}
		if h.app.flagsOf(command) == nil {
			t.Errorf("%s: its flag set was not found", command)
		}
	}
	if h.stdout.Len()+h.stderr.Len() != 0 {
		t.Errorf("reading flags printed: %q %q", h.stdout, h.stderr)
	}
}

func TestCompletionPrintsAScriptPerShell(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		h := newHarness(t, nil)
		if code := h.run("completion", shell); code != 0 {
			t.Fatalf("%s: exit %d: %s", shell, code, h.stderr)
		}
		if !strings.Contains(h.stdout.String(), "cw __complete") {
			t.Errorf("%s:\n%s", shell, h.stdout)
		}
	}
	h := newHarness(t, nil)
	if code := h.run("completion", "powershell"); code != 2 {
		t.Errorf("exit %d", code)
	}
}

func contains(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}
