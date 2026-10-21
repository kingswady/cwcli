package commands

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// completeCommand is what the shell scripts ask on every Tab: the words typed
// after "cw", the last one being completed. It is not in the usage.
const completeCommand = "__complete"

var completionScripts = map[string]string{
	"bash": `# cw completion for bash. Add to ~/.bashrc:  source <(cw completion bash)
_cw_complete() {
    local IFS=$'\n'
    COMPREPLY=($(cw __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null))
}
complete -F _cw_complete cw
`,
	"zsh": `# cw completion for zsh. Add to ~/.zshrc, after compinit:  source <(cw completion zsh)
_cw() {
    local -a candidates
    candidates=(${(f)"$(cw __complete "${(@)words[2,CURRENT]}" 2>/dev/null)"})
    compadd -a candidates
}
compdef _cw cw
`,
	"fish": `# cw completion for fish:  cw completion fish > ~/.config/fish/completions/cw.fish
function __cw_complete
    set -l before (commandline -opc)
    set -l current (commandline -ct)
    cw __complete $before[2..-1] "$current" 2>/dev/null
end
complete -c cw -f -a '(__cw_complete)'
`,
}

func (a *App) completion(args []string) error {
	fs := a.newFlags("completion")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, `Usage: cw completion bash|zsh|fish

Prints the shell's completion script for cw: Tab completes commands, show,
each command's flags and the values some flags take (--env, --source, …).

  bash   in ~/.bashrc:              source <(cw completion bash)
  zsh    in ~/.zshrc, after compinit: source <(cw completion zsh)
  fish   cw completion fish > ~/.config/fish/completions/cw.fish
`)
	}
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || completionScripts[positional[0]] == "" {
		fs.Usage()
		return usagef("name a shell: bash, zsh or fish")
	}
	_, err = io.WriteString(a.stdout, completionScripts[positional[0]])
	return err
}

// complete prints the candidates for the last of words, one per line. It
// never asks the platform: a Tab must answer at once, also offline.
func (a *App) complete(words []string) int {
	for _, candidate := range a.candidates(words) {
		fmt.Fprintln(a.stdout, candidate)
	}
	return 0
}

// flagValues are the values a flag takes when there are few and fixed.
var flagValues = map[string][]string{
	"env":     {"production", "staging", "development"},
	"edition": {"community", "enterprise"},
	"color":   {"auto", "always", "never"},
	"source":  {"main", "setup", "restore", "backup", "transfer", "upgrade", "script", "all"},
	"ssl":     {"valid", "expiring_soon", "expired", "unknown"},
	"format":  {"zip", "dump"},
	// cw apps update and cw apps update-modules
	"modules":       {"changed", "configured", "none"},
	"backup-format": {"zip", "dump"},
}

func (a *App) candidates(words []string) []string {
	if len(words) == 0 {
		words = []string{""}
	}
	current, before := words[len(words)-1], words[:len(words)-1]
	if len(before) == 0 {
		return withPrefix(commandNames(), current)
	}
	command := before[0]
	if command == "runs" && len(before) > 1 && before[1] == "retry" {
		// cw runs retry: its own flags, after the subcommand word.
		command, before = "runs retry", append([]string{"runs retry"}, before[2:]...)
	}
	if command == "apps" && len(before) > 1 && (before[1] == "update" || before[1] == "update-modules") {
		// cw apps update / update-modules: their own flags, after the subcommand word.
		sub := "apps " + before[1]
		command, before = sub, append([]string{sub}, before[2:]...)
	}
	fs := a.flagsOf(command)
	if fs == nil {
		return nil
	}
	if name, partial, ok := strings.Cut(current, "="); ok && strings.HasPrefix(name, "-") {
		return withPrefix(prefixed(name+"=", flagValues[strings.TrimLeft(name, "-")]), name+"="+partial)
	}
	if f := valueFlag(fs, before[1:]); f != nil {
		return withPrefix(flagValues[f.Name], current)
	}
	if strings.HasPrefix(current, "-") {
		var names []string
		fs.VisitAll(func(f *flag.Flag) { names = append(names, "--"+f.Name) })
		return withPrefix(names, current)
	}
	switch positional := positionals(fs, before[1:]); {
	case command == "completion" && len(positional) == 0:
		return withPrefix([]string{"bash", "zsh", "fish"}, current)
	case command == "runs" && len(positional) == 0:
		return withPrefix([]string{"retry", "show"}, current)
	case command == "apps" && len(positional) == 0:
		return withPrefix([]string{"show", "update", "update-modules"}, current)
	case command == "maintenance" && len(positional) == 1:
		return withPrefix([]string{"on", "off"}, current)
	case len(positional) == 0:
		if res := resourceNamed(command); res != nil && !res.noShow {
			return withPrefix([]string{"show"}, current)
		}
	}
	return nil
}

// commandNames is every command a person types, sorted.
func commandNames() []string {
	names := []string{
		"login", "logout", "whoami", "namespaces", "use", "update", "completion",
		"attention", "restore-points", "logs", "mcp", "help", "version", "backup", "restart",
		"maintenance", "ssh",
	}
	for _, res := range resources {
		names = append(names, res.name)
	}
	sort.Strings(names)
	return names
}

// flagsOf is the flags command defines, read by running it with -h on a
// quiet copy of the app: every command defines its flags before it does
// anything, so nothing runs but the parse. Nil for a command without flags.
func (a *App) flagsOf(command string) *flag.FlagSet {
	if command == completeCommand {
		return nil
	}
	quiet := *a
	quiet.stdout, quiet.stderr = io.Discard, io.Discard
	var defined *flag.FlagSet
	quiet.flagsDefined = func(fs *flag.FlagSet) {
		if defined == nil {
			defined = fs
		}
	}
	name, args := command, []string{"-h"}
	switch command {
	case "runs retry":
		name, args = "runs", []string{"retry", "-h"}
	case "apps update", "apps update-modules":
		name, args = "apps", []string{strings.TrimPrefix(command, "apps "), "-h"}
	}
	quiet.dispatch(name, args)
	return defined
}

// valueFlag is the flag whose value is being typed: the last word is a flag
// that takes one ("--env "), or bash split "--env=" into "--env" and "=".
func valueFlag(fs *flag.FlagSet, words []string) *flag.Flag {
	n := len(words)
	if n >= 2 && words[n-1] == "=" {
		words = words[:n-1]
		n--
	}
	if n == 0 || !strings.HasPrefix(words[n-1], "-") || strings.Contains(words[n-1], "=") {
		return nil
	}
	f := fs.Lookup(strings.TrimLeft(words[n-1], "-"))
	if f == nil || isBoolFlag(f) {
		return nil
	}
	return f
}

// positionals is what of words is not a flag or a flag's value.
func positionals(fs *flag.FlagSet, words []string) []string {
	var found []string
	for i := 0; i < len(words); i++ {
		word := words[i]
		if !strings.HasPrefix(word, "-") {
			found = append(found, word)
			continue
		}
		if f := fs.Lookup(strings.TrimLeft(word, "-")); f != nil && !isBoolFlag(f) && !strings.Contains(word, "=") {
			i++ // its value
		}
	}
	return found
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func prefixed(prefix string, values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = prefix + v
	}
	return out
}

func withPrefix(candidates []string, prefix string) []string {
	var matching []string
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, prefix) {
			matching = append(matching, candidate)
		}
	}
	return matching
}
