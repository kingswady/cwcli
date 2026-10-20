package commands

import (
	"encoding/json"
	"strings"
	"testing"
)

func readReference(t *testing.T) reference {
	t.Helper()
	h := newHarness(t, nil)
	if code := h.run(referenceCommand, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	var ref reference
	if err := json.Unmarshal(h.stdout.Bytes(), &ref); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, h.stdout)
	}
	return ref
}

func TestReferenceHasEveryCommandInTheHelp(t *testing.T) {
	ref := readReference(t)
	named := map[string]refCommand{}
	for _, command := range ref.Commands {
		if _, twice := named[command.Name]; twice {
			t.Errorf("%s listed twice", command.Name)
		}
		named[command.Name] = command
		if command.Summary == "" || command.Group == "" || !strings.HasPrefix(command.Usage, "cw ") {
			t.Errorf("%s: summary %q, group %q, usage %q", command.Name, command.Summary, command.Group, command.Usage)
		}
	}
	for _, name := range append(commandNames(), subcommands...) {
		if name == "help" || name == "version" {
			continue
		}
		if _, ok := named[name]; !ok {
			t.Errorf("the help lists %s; the reference does not", name)
		}
	}
}

func TestReferenceCarriesFlagsUsageAndExamples(t *testing.T) {
	ref := readReference(t)
	var update refCommand
	for _, command := range ref.Commands {
		if command.Name == "apps update" {
			update = command
		}
	}
	if update.Group != "Change" || !strings.HasPrefix(update.Usage, "cw apps update <app>") {
		t.Fatalf("apps update: %+v", update)
	}
	flags := map[string]refFlag{}
	for _, f := range update.Flags {
		flags[f.Name] = f
	}
	if flags["modules"].Default != "changed" || flags["modules"].Usage == "" || flags["quick"].Name == "" {
		t.Errorf("flags %+v", update.Flags)
	}
	if len(update.Examples) == 0 || !strings.HasPrefix(update.Examples[0], "cw apps update <app>") {
		t.Errorf("examples %v", update.Examples)
	}
	if update.Description == "" {
		t.Error("no description from its -h")
	}
}

func TestReferenceIsHiddenAndNeedsJSON(t *testing.T) {
	h := newHarness(t, nil)
	if code := h.run(referenceCommand); code != 2 {
		t.Errorf("without --json: exit %d", code)
	}
	for _, name := range commandNames() {
		if name == referenceCommand {
			t.Error("the reference is a hidden command")
		}
	}
}
