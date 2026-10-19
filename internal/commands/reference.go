package commands

// cw __reference --json — every command, its usage, flags and examples, as the
// platform's generated CLI reference shows them (/docs/reference/cli). Hidden:
// not in the help, never completed. The release publishes its output as
// cw-reference.json beside the archives, listed in the signed checksums.txt.
//
// The commands and their summaries come from the help text itself (usage), so
// the reference and the help cannot disagree: TestReferenceHasEveryCommandInTheHelp.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// referenceCommand is cw __reference: like __complete, a word no one types.
const referenceCommand = "__reference"

// referenceGroups are the help's sections that list commands, in their order.
var referenceGroups = []string{"Account", "Read", "Change", "AI agents"}

// subcommands are the commands of two words the help lists.
var subcommands = []string{"runs retry", "apps update", "apps update-modules"}

// continuationSummaries name what a help line without a command of its own
// ("…or some of its modules") stands for, as a sentence of its own.
var continuationSummaries = map[string]string{
	"apps update-modules": "Update some of an app's modules",
}

var helpColumns = regexp.MustCompile(`\s{2,}`)

type refFlag struct {
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"`
	Default string `json:"default,omitempty"`
	Usage   string `json:"usage"`
}

type refCommand struct {
	Name        string    `json:"name"`
	Group       string    `json:"group"`
	Summary     string    `json:"summary"`
	Usage       string    `json:"usage"`
	Description string    `json:"description,omitempty"`
	Flags       []refFlag `json:"flags"`
	Examples    []string  `json:"examples"`
}

type reference struct {
	Version  string       `json:"version"`
	Commands []refCommand `json:"commands"`
}

func (a *App) reference(args []string) error {
	fs := flag.NewFlagSet("cw "+referenceCommand, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	asJSON := fs.Bool("json", false, "print the reference as JSON (the only form)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*asJSON {
		return usagef("cw %s takes --json", referenceCommand)
	}
	encoder := json.NewEncoder(a.stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(reference{Version: a.version, Commands: a.referenceCommands()})
}

// helpEntry is one command line of the help: its section, name, summary and example.
type helpEntry struct {
	group, name, summary, example string
}

// helpEntries are the command lines of the help text, in its order.
func (a *App) helpEntries() []helpEntry {
	var text bytes.Buffer
	a.usage(&text)
	known := map[string]bool{}
	for _, name := range append(commandNames(), subcommands...) {
		known[name] = true
	}
	var entries []helpEntry
	group := ""
	for _, line := range strings.Split(text.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			group = sectionOf(line)
			continue
		}
		if group == "" || strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "(") {
			continue
		}
		if entry, ok := helpLine(line, group, known); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// sectionOf is the help section a heading opens ("Change (asks first…)" → Change), or "".
func sectionOf(heading string) string {
	for _, group := range referenceGroups {
		if strings.HasPrefix(heading, group) {
			return group
		}
	}
	return ""
}

// helpLine reads "  name   summary   example": a continuation line (no name of
// its own) is the command its example runs.
func helpLine(line, group string, known map[string]bool) (helpEntry, bool) {
	columns := helpColumns.Split(strings.TrimSpace(line), -1)
	entry := helpEntry{group: group}
	if strings.HasPrefix(line, "   ") {
		entry.example = exampleIn(columns[len(columns)-1])
		entry.name = commandOf(entry.example, known)
		entry.summary = continuationSummaries[entry.name]
		return entry, entry.name != ""
	}
	if !known[columns[0]] {
		return entry, false
	}
	entry.name = columns[0]
	if len(columns) > 1 {
		entry.summary = columns[1]
	}
	if len(columns) > 2 {
		entry.example = exampleIn(columns[2])
	}
	return entry, true
}

// exampleIn is the "cw …" command a help column shows, up to its comment, or "".
func exampleIn(column string) string {
	at := strings.Index(column, "cw ")
	if at < 0 {
		return ""
	}
	example := column[at:]
	if colon := strings.Index(example, ": "); colon > 0 {
		example = example[:colon]
	}
	return strings.TrimSpace(example)
}

// commandOf is the longest known command an example runs: "cw apps update-modules …" → apps update-modules.
func commandOf(example string, known map[string]bool) string {
	words := strings.Fields(strings.TrimPrefix(example, "cw "))
	for n := min(2, len(words)); n > 0; n-- {
		if name := strings.Join(words[:n], " "); known[name] {
			return name
		}
	}
	return ""
}

func (a *App) referenceCommands() []refCommand {
	var commands []refCommand
	for _, entry := range a.helpEntries() {
		usage, description := a.usageOf(entry.name)
		command := refCommand{
			Name:        entry.name,
			Group:       entry.group,
			Summary:     entry.summary,
			Usage:       usage,
			Description: description,
			Flags:       a.referenceFlags(entry.name),
			Examples:    []string{},
		}
		if entry.example != "" {
			command.Examples = append(command.Examples, entry.example)
		}
		commands = append(commands, command)
	}
	return commands
}

// referenceFlags are a command's flags, as completion reads them, by name.
func (a *App) referenceFlags(name string) []refFlag {
	flags := []refFlag{}
	fs := a.flagsOf(name)
	if fs == nil {
		return flags
	}
	fs.VisitAll(func(f *flag.Flag) {
		value, usage := flag.UnquoteUsage(f)
		entry := refFlag{Name: f.Name, Value: value, Usage: usage}
		if !isZeroDefault(f) {
			entry.Default = f.DefValue
		}
		flags = append(flags, entry)
	})
	sort.Slice(flags, func(i, j int) bool { return flags[i].Name < flags[j].Name })
	return flags
}

func isZeroDefault(f *flag.Flag) bool {
	switch f.DefValue {
	case "", "0", "false", "0s":
		return true
	}
	return false
}

// usageOf is the usage line a command's -h prints, and the words under it.
func (a *App) usageOf(name string) (usage, description string) {
	var printed bytes.Buffer
	quiet := *a
	quiet.stdout, quiet.stderr = io.Discard, &printed
	words := strings.Fields(name)
	quiet.dispatch(words[0], append(words[1:], "-h"))
	usage, description = parseUsage(printed.String())
	if usage == "" {
		usage = fmt.Sprintf("cw %s [flags]", name)
	}
	return usage, description
}

// parseUsage reads "Usage: cw …" and the paragraphs before "Flags:".
func parseUsage(text string) (usage, description string) {
	lines := strings.Split(text, "\n")
	var about []string
	for i, line := range lines {
		if after, ok := strings.CutPrefix(line, "Usage: "); ok && usage == "" {
			usage = strings.TrimSpace(after)
			continue
		}
		if usage == "" {
			continue
		}
		if strings.HasPrefix(line, "Flags:") || strings.HasPrefix(line, "  -") {
			break
		}
		if i > 0 {
			about = append(about, line)
		}
	}
	return usage, strings.TrimSpace(strings.Join(about, "\n"))
}
