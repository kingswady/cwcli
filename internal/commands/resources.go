package commands

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

type record = map[string]any

// field is one value of a record: its JSON key, its label and how it prints.
type field struct {
	key    string
	title  string
	format func(any) string
	// hideEmpty drops the column from a table where no row has a value
	// (an older platform without the field, runs that are not about an app).
	hideEmpty bool
}

func (f field) render(r record) string {
	if f.format != nil {
		return f.format(r[f.key])
	}
	return output.Text(r[f.key])
}

// filter is a flag that narrows a list; lookup names the resource whose name
// the flag may be given instead of an id ("--app shop").
type filter struct {
	flag   string
	param  string
	usage  string
	lookup string
	// narrows is the filter as a command that names a record of this resource
	// offers it, to tell apart records that share the name ("--app v19-0
	// --project internal"): "an app on this server (id or name)". Empty: the
	// filter is only the list's own — never offered where it would collide
	// with the other command's filters (--state on runs is the run's state).
	narrows string
}

type resource struct {
	name     string // command and path: "apps" → /apps
	singular string
	// byName: "show" and filters accept this resource's name, not only its id.
	byName  bool
	filters []filter
	columns []field
	details []field
	// extra prints what a detail view has beyond its fields (a run's steps).
	extra func(a *App, r record, out output.Table) error
	// hidesDeleted: the API leaves deleted and disabled records out unless asked (--all).
	hidesDeleted bool
	// optional columns, each shown when its own flag is given (--show-url).
	optional []optionalColumn
	// nameSearch is the list parameter that finds the records whose name
	// contains a text: a lookup by name asks for it rather than paging through
	// every record. Empty: the lookup pages through the (filtered) list.
	nameSearch string
}

type optionalColumn struct {
	flag  string
	usage string
	field field
}

var namespaceField = field{key: "namespace", title: "NAMESPACE"}

// projectField is the app's project — what an app belongs to, quicker than its name.
var projectField = field{key: "project", title: "PROJECT", hideEmpty: true}

// envField is the app's environment, coloured like the dashboard.
var envField = field{key: "environment_type", title: "ENV", hideEmpty: true}

var resources = []*resource{
	{
		name: "apps", singular: "app", byName: true, hidesDeleted: true, nameSearch: "search",
		optional: []optionalColumn{{flag: "show-url", usage: "add the URL column", field: field{key: "url", title: "URL"}}},
		filters: []filter{
			{flag: "search", param: "search", usage: "only apps whose name contains this (any case)"},
			{flag: "project", param: "project", usage: "only apps whose project name or code contains this",
				narrows: "an app whose project name or code contains this"},
			{flag: "env", param: "environment_type", usage: "production, staging or development",
				narrows: "an app in this environment (production, staging or development)"},
			{flag: "server", param: "server_id", usage: "only apps on this server (id or name)", lookup: "servers",
				narrows: "an app on this server (id or name)"},
			{flag: "version", param: "version", usage: "only this Odoo version, e.g. 19.0",
				narrows: "an app of this Odoo version, e.g. 19.0"},
			{flag: "edition", param: "edition", usage: "community or enterprise",
				narrows: "an app of this edition (community or enterprise)"},
			{flag: "state", param: "state", usage: "only apps in this state"},
		},
		columns: []field{
			{key: "id", title: "ID"}, {key: "name", title: "NAME"}, projectField,
			{key: "environment_type", title: "ENV"}, {key: "version", title: "VERSION"},
			{key: "state", title: "STATE"}, {key: "server", title: "SERVER"},
			{key: "backup_health", title: "BACKUPS"}, {key: "updated_at", title: "UPDATED", format: output.Ago},
			namespaceField,
		},
		details: []field{
			{key: "id", title: "ID"}, {key: "name", title: "Name"}, {key: "namespace", title: "Namespace"},
			{key: "project", title: "Project"}, {key: "environment", title: "Environment"},
			{key: "environment_type", title: "Type"}, {key: "version", title: "Version"},
			{key: "edition", title: "Edition"}, {key: "state", title: "State"}, {key: "url", title: "URL"},
			{key: "server", title: "Server"}, {key: "deployed_at", title: "Deployed", format: output.LocalTime},
			{key: "updated_at", title: "Updated", format: output.LocalTime},
			{key: "backup_health", title: "Backup health"},
			{key: "last_backup_at", title: "Last backup", format: output.LocalTime},
		},
	},
	{
		name: "servers", singular: "server", byName: true, hidesDeleted: true,
		filters: []filter{{flag: "state", param: "state", usage: "only servers in this state"}},
		columns: []field{
			{key: "id", title: "ID"}, {key: "name", title: "NAME"}, {key: "state", title: "STATE"},
			{key: "provider", title: "PROVIDER"}, {key: "region", title: "REGION"}, {key: "size", title: "SIZE"},
			{key: "public_ip", title: "IP"}, {key: "app_count", title: "APPS"}, namespaceField,
		},
		details: []field{
			{key: "id", title: "ID"}, {key: "name", title: "Name"}, {key: "namespace", title: "Namespace"},
			{key: "state", title: "State"}, {key: "provider", title: "Provider"}, {key: "size", title: "Size"},
			{key: "region", title: "Region"}, {key: "image", title: "Image"},
			{key: "lb_engine", title: "Load balancer"}, {key: "public_ip", title: "Public IP"},
			{key: "app_count", title: "Apps"},
		},
	},
	{
		name: "installers", singular: "installer", byName: true, hidesDeleted: true,
		filters: []filter{
			{flag: "server", param: "server_id", usage: "only on this server (id or name)", lookup: "servers"},
			{flag: "state", param: "state", usage: "only installers in this state"},
		},
		columns: []field{
			{key: "id", title: "ID"}, {key: "name", title: "NAME"}, {key: "type", title: "TYPE"},
			{key: "state", title: "STATE"}, {key: "server", title: "SERVER"}, {key: "url", title: "URL"},
			namespaceField,
		},
		details: []field{
			{key: "id", title: "ID"}, {key: "name", title: "Name"}, {key: "namespace", title: "Namespace"},
			{key: "type", title: "Type"}, {key: "app", title: "Marketplace app"}, {key: "state", title: "State"},
			{key: "server", title: "Server"}, {key: "url", title: "URL"},
			{key: "platform_login", title: "Platform login"},
		},
	},
	{
		name: "backups", singular: "backup",
		filters: []filter{{flag: "app", param: "app_id", usage: "only this app's backups (id or name)", lookup: "apps"}},
		columns: []field{
			{key: "id", title: "ID"}, {key: "app", title: "APP"}, projectField, envField,
			{key: "taken_at", title: "TAKEN", format: output.Ago},
			{key: "size_mb", title: "SIZE", format: output.Megabytes}, {key: "format", title: "FORMAT"},
			{key: "automated", title: "AUTO"}, {key: "state", title: "STATE"}, {key: "storage", title: "STORAGE"},
			namespaceField,
		},
		details: []field{
			{key: "id", title: "ID"}, {key: "name", title: "Name"}, {key: "namespace", title: "Namespace"},
			{key: "app", title: "App"}, {key: "project", title: "Project"}, {key: "environment_type", title: "Environment"},
			{key: "taken_at", title: "Taken", format: output.LocalTime}, {key: "size_mb", title: "Size", format: output.Megabytes},
			{key: "format", title: "Format"}, {key: "automated", title: "Automated"}, {key: "state", title: "State"},
			{key: "storage", title: "Storage"},
		},
	},
	{
		name: "runs", singular: "run",
		filters: []filter{
			{flag: "app", param: "app_id", usage: "only this app's runs (id or name)", lookup: "apps"},
			{flag: "state", param: "state", usage: "only runs in this state (e.g. error, running, done)"},
		},
		columns: []field{
			{key: "id", title: "ID"}, {key: "workflow", title: "WORKFLOW"}, {key: "action", title: "ACTION"},
			{key: "state", title: "STATE"}, {key: "record", title: "FOR"}, projectField, envField,
			{key: "updated_at", title: "UPDATED", format: output.Ago}, namespaceField,
		},
		details: []field{
			{key: "id", title: "ID"}, {key: "workflow", title: "Workflow"}, {key: "action", title: "Action"},
			{key: "state", title: "State"}, {key: "namespace", title: "Namespace"}, {key: "record", title: "For"},
			{key: "record_type", title: "Type"}, {key: "project", title: "Project"}, {key: "environment_type", title: "Environment"},
			{key: "created_at", title: "Started", format: output.LocalTime},
			{key: "updated_at", title: "Updated", format: output.LocalTime},
		},
		extra: printSteps,
	},
}

func resourceNamed(name string) *resource {
	for _, r := range resources {
		if r.name == name {
			return r
		}
	}
	return nil
}

// readFlags are the flags every read command takes.
type readFlags struct {
	json      bool
	namespace string
	limit     int
	offset    int
	color     string
	all       bool
	filters   map[string]*string
	optional  map[string]*bool
	// named is the filter whose value names another resource's record and
	// whose lookup the narrow flags narrow ("app": --app v19-0 --project internal).
	named  *filter
	narrow map[string]*string
}

func (a *App) readFlagSet(res *resource) (*flag.FlagSet, *readFlags) {
	fs := a.newFlags(res.name)
	opts := &readFlags{filters: map[string]*string{}, optional: map[string]*bool{}}
	fs.BoolVar(&opts.json, "json", false, "print the API response as JSON")
	fs.StringVar(&opts.color, "color", "auto", "auto (in a terminal, unless NO_COLOR is set), always or never")
	if res.hidesDeleted {
		fs.BoolVar(&opts.all, "all", false, "include deleted and disabled "+res.name)
	}
	for _, column := range res.optional {
		opts.optional[column.flag] = fs.Bool(column.flag, false, column.usage)
	}
	fs.StringVar(&opts.namespace, "namespace", "", "only this namespace (code or id)")
	fs.IntVar(&opts.limit, "limit", 50, "rows per page (1-200)")
	fs.IntVar(&opts.offset, "offset", 0, "rows to skip")
	for i, f := range res.filters {
		opts.filters[f.flag] = fs.String(f.flag, "", f.usage)
		if f.lookup != "" && len(narrowing(resourceNamed(f.lookup))) > 0 {
			opts.named = &res.filters[i]
			opts.narrow = narrowFlags(fs, resourceNamed(f.lookup), "--"+f.flag)
		}
	}
	fs.Usage = func() {
		show := "<id>"
		if res.byName {
			show = "<id|name>"
		}
		fmt.Fprintf(a.stderr, "Usage: cw %s [flags]\n       cw %s show %s [flags]\n", res.name, res.name, show)
		if res.byName && len(res.filters) > 0 {
			fmt.Fprintf(a.stderr, "\nA name several %s share is narrowed by the filters below (all but --%s):\n"+
				"  cw %s show <name> --%s <value>\n", res.name, searchFlag(res), res.name, showFilters(res)[0])
		}
		if opts.named != nil {
			target := resourceNamed(opts.named.lookup)
			fmt.Fprintf(a.stderr, "\n--%s takes an %s's id or name. Where several %s share the name,\n"+
				"%s narrow which one it means (and nothing else).\n",
				opts.named.flag, target.singular, target.name, joinFlags(narrowing(target), "and"))
		}
		fmt.Fprintln(a.stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	return fs, opts
}

// narrowing is the flags of res's filters another command offers to narrow a
// name of res: the ones with a narrows text, in the order they are declared.
func narrowing(res *resource) []string {
	var flags []string
	for _, f := range res.filters {
		if f.narrows != "" {
			flags = append(flags, f.flag)
		}
	}
	return flags
}

// narrowFlags adds target's narrowing filters to fs. They narrow which record
// the name given to named ("--app", "<app>") means, and filter nothing else.
func narrowFlags(fs *flag.FlagSet, target *resource, named string) map[string]*string {
	values := map[string]*string{}
	for _, f := range target.filters {
		if f.narrows != "" {
			values[f.flag] = fs.String(f.flag, "", "narrows "+named+" to "+f.narrows)
		}
	}
	return values
}

// given is the flags of values that were set, flag → value.
func given(values map[string]*string) map[string]string {
	set := map[string]string{}
	for flag, value := range values {
		if *value != "" {
			set[flag] = *value
		}
	}
	return set
}

// showFilters is the filters show narrows a name with: all but the name search.
func showFilters(res *resource) []string {
	var flags []string
	for _, f := range res.filters {
		if f.param != res.nameSearch {
			flags = append(flags, f.flag)
		}
	}
	return flags
}

func searchFlag(res *resource) string {
	for _, f := range res.filters {
		if f.param == res.nameSearch {
			return f.flag
		}
	}
	return ""
}

func (a *App) resource(res *resource, args []string) error {
	fs, opts := a.readFlagSet(res)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if opts.named != nil && *opts.filters[opts.named.flag] == "" {
		target := resourceNamed(opts.named.lookup)
		if stray := sortedFlags(given(opts.narrow), narrowing(target)); len(stray) > 0 {
			verb := "narrow"
			if len(stray) == 1 {
				verb = "narrows"
			}
			return usagef("%s only %s which %s --%s names; add --%s <name>",
				joinFlags(stray, "and"), verb, target.singular, opts.named.flag, opts.named.flag)
		}
	}
	color, err := a.useColor(opts.color, opts.json)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	out := output.Table{Color: color}
	switch {
	case len(positional) == 0:
		return a.list(c, res, opts, out)
	case positional[0] == "show" && len(positional) == 2:
		return a.show(c, res, opts, positional[1], out)
	default:
		fs.Usage()
		return usagef("unexpected arguments: %s", strings.Join(positional, " "))
	}
}

// columnsFor is the resource's columns plus the optional ones asked for,
// placed before the trailing namespace column.
func columnsFor(res *resource, opts *readFlags) []field {
	columns := append([]field{}, res.columns...)
	for _, column := range res.optional {
		if !*opts.optional[column.flag] {
			continue
		}
		at := len(columns)
		if at > 0 && columns[at-1].key == namespaceField.key {
			at--
		}
		columns = append(columns[:at], append([]field{column.field}, columns[at:]...)...)
	}
	return columns
}

// getList asks for one list page. A platform older than include_deleted and
// search answers unknown_parameter; the optional parameters — ones that only
// save work there (an older platform already lists deleted records; a lookup
// matches names itself) — are dropped and the page asked for again.
func getList(c *platform.Client, path string, query url.Values, optional ...string) (json.RawMessage, error) {
	raw, err := c.Get(path, query)
	var refused *platform.APIError
	if !errors.As(err, &refused) || refused.Code != "unknown_parameter" {
		return raw, err
	}
	dropped := false
	for _, param := range optional {
		if param != "" && query.Has(param) {
			query.Del(param)
			dropped = true
		}
	}
	if !dropped {
		return raw, err
	}
	return c.Get(path, query)
}

// scope is what narrows a list, or a lookup by name: the namespace, deleted
// records, and the resource's filters that were given (flag → value).
type scope struct {
	namespace      string
	includeDeleted bool
	filters        map[string]string
	// narrow narrows the lookup of the name a filter names (--app v19-0):
	// the looked-up resource's filters, flag → value.
	narrow map[string]string
	// offered is every flag that narrows this lookup, for the hint when a
	// name is still ambiguous (--namespace is always offered).
	offered []string
}

// query is the list query of res in s. A filter given a name (--server
// prod-1) is looked up first, narrowed by s.narrow.
func (a *App) query(c *platform.Client, res *resource, s scope, out output.Table) (url.Values, error) {
	query := url.Values{}
	if s.namespace != "" {
		query.Set("namespace", s.namespace)
	}
	if s.includeDeleted && res.hidesDeleted {
		query.Set("include_deleted", "true")
	}
	for _, f := range res.filters {
		value := s.filters[f.flag]
		if value == "" {
			continue
		}
		if f.lookup != "" {
			target := resourceNamed(f.lookup)
			id, err := a.resolveID(c, target, value, scope{namespace: s.namespace, filters: s.narrow, offered: narrowing(target)}, out)
			if err != nil {
				return nil, err
			}
			value = id
		}
		query.Set(f.param, value)
	}
	return query, nil
}

func (a *App) list(c *platform.Client, res *resource, opts *readFlags, out output.Table) error {
	query, err := a.query(c, res, scope{
		namespace: opts.namespace, includeDeleted: opts.all, filters: given(opts.filters), narrow: given(opts.narrow),
	}, out)
	if err != nil {
		return err
	}
	query.Set("limit", strconv.Itoa(opts.limit))
	query.Set("offset", strconv.Itoa(opts.offset))
	raw, err := getList(c, "/"+res.name, query, "include_deleted")
	if err != nil {
		return err
	}
	if opts.json {
		return output.WriteJSON(a.stdout, raw)
	}
	var page struct {
		Items []record    `json:"items"`
		Total json.Number `json:"total"`
	}
	if err := platform.Decode(raw, &page); err != nil {
		return err
	}
	if len(page.Items) == 0 {
		fmt.Fprintf(a.stderr, "No %s.\n", res.name)
		return nil
	}
	if err := writeRecords(a.stdout, columnsFor(res, opts), page.Items, out); err != nil {
		return err
	}
	if total, _ := page.Total.Int64(); int(total) > opts.offset+len(page.Items) {
		fmt.Fprintf(a.stderr, "\nShowing %d-%d of %d — next page: --offset %d\n",
			opts.offset+1, opts.offset+len(page.Items), total, opts.offset+len(page.Items))
	}
	return nil
}

// writeRecords prints items as a table of columns (the namespace column only
// when they span more than one).
func writeRecords(w io.Writer, columns []field, items []record, out output.Table) error {
	columns = visibleColumns(columns, items)
	headers := make([]string, len(columns))
	for i, col := range columns {
		headers[i] = col.title
	}
	rows := make([][]string, len(items))
	for i, item := range items {
		rows[i] = make([]string, len(columns))
		for j, col := range columns {
			rows[i][j] = out.Cell(col.key, col.render(item))
		}
	}
	return output.WriteTable(w, out.Headers(headers), rows)
}

// visibleColumns drops the namespace column when every row shares one, and a
// hideEmpty column when no row has a value.
func visibleColumns(columns []field, items []record) []field {
	visible := make([]field, 0, len(columns))
	for _, col := range columns {
		seen := map[string]bool{}
		for _, item := range items {
			seen[output.Text(item[col.key])] = true
		}
		switch {
		case col.key == namespaceField.key && len(seen) < 2:
		case col.hideEmpty && len(seen) == 1 && seen["-"]:
		default:
			visible = append(visible, col)
		}
	}
	return visible
}

func (a *App) show(c *platform.Client, res *resource, opts *readFlags, ref string, out output.Table) error {
	// The list's filters narrow the name (cw apps show v19-0 --project internal);
	// the name search is the name itself.
	if flag := searchFlag(res); flag != "" && *opts.filters[flag] != "" {
		return usagef("--%s is for the list; show takes one %s's exact name (or its id)", flag, res.singular)
	}
	id, err := a.resolveID(c, res, ref, scope{
		namespace: opts.namespace, includeDeleted: opts.all, filters: given(opts.filters), offered: showFilters(res),
	}, out)
	if err != nil {
		return err
	}
	raw, err := c.Get("/"+res.name+"/"+id, nil)
	if err != nil {
		return err
	}
	if opts.json {
		return output.WriteJSON(a.stdout, raw)
	}
	var item record
	if err := platform.Decode(raw, &item); err != nil {
		return err
	}
	rows := make([][]string, len(res.details))
	for i, f := range res.details {
		rows[i] = []string{out.Label(f.title + ":"), out.Cell(f.key, f.render(item))}
	}
	if err := output.WriteTable(a.stdout, nil, rows); err != nil {
		return err
	}
	if res.extra != nil {
		return res.extra(a, item, out)
	}
	return nil
}

// lookupPage is the page size a lookup by name reads (the API's maximum).
const lookupPage = 200

// resolveID accepts an id, or the exact name (any case) of one record the
// token can see in scope s. Deleted and disabled records are left out unless
// s.includeDeleted (--all); their ids still work. A resource with a name
// search is asked only for the names containing ref; the others are paged
// through, filtered by s.
func (a *App) resolveID(c *platform.Client, res *resource, ref string, s scope, out output.Table) (string, error) {
	if _, err := strconv.Atoi(ref); err == nil {
		return ref, nil
	}
	if !res.byName {
		return "", usagef("%s are looked up by id, not by name: %q", res.name, ref)
	}
	query, err := a.query(c, res, s, out)
	if err != nil {
		return "", err
	}
	if res.nameSearch != "" {
		query.Set(res.nameSearch, ref)
	}
	matches, err := namedIn(c, res, ref, query)
	if err != nil {
		return "", err
	}
	applied := s.applied(res)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no %s named %q%s in this token's namespaces", res.singular, ref, applied)
	case 1:
		return output.Text(matches[0]["id"]), nil
	}
	// Several match: show them the way cw shows records, on stderr (stdout may be --json).
	fmt.Fprintf(a.stderr, "%d %ss are named %q%s:\n\n", len(matches), res.singular, ref, applied)
	if err := writeRecords(a.stderr, res.columns, matches, out); err != nil {
		return "", err
	}
	fmt.Fprintln(a.stderr)
	if further := s.further(); len(further) > 0 {
		return "", fmt.Errorf("use one of these ids instead of %q, or narrow it with %s", ref, joinFlags(further, "or"))
	}
	return "", fmt.Errorf("use one of these ids instead of %q", ref)
}

// namedIn pages through res's list for query and keeps the records named ref.
func namedIn(c *platform.Client, res *resource, ref string, query url.Values) ([]record, error) {
	var matches []record
	for offset := 0; ; offset += lookupPage {
		query.Set("limit", strconv.Itoa(lookupPage))
		query.Set("offset", strconv.Itoa(offset))
		raw, err := getList(c, "/"+res.name, query, "include_deleted", res.nameSearch)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items []record    `json:"items"`
			Total json.Number `json:"total"`
		}
		if err := platform.Decode(raw, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if strings.EqualFold(output.Text(item["name"]), ref) {
				matches = append(matches, item)
			}
		}
		if total, _ := page.Total.Int64(); len(page.Items) == 0 || int64(offset+lookupPage) >= total {
			return matches, nil
		}
	}
}

// applied is s as the flags that were given, " with --project internal", or "".
func (s scope) applied(res *resource) string {
	var parts []string
	if s.namespace != "" {
		parts = append(parts, flagText("namespace", s.namespace))
	}
	for _, f := range res.filters {
		if value := s.filters[f.flag]; value != "" {
			parts = append(parts, flagText(f.flag, value))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " with " + strings.Join(parts, " ")
}

// further is the narrowing flags not given yet, --namespace last.
func (s scope) further() []string {
	var flags []string
	for _, flag := range s.offered {
		if s.filters[flag] == "" {
			flags = append(flags, flag)
		}
	}
	if s.namespace == "" {
		flags = append(flags, "namespace")
	}
	return flags
}

// flagText is a flag as it would be typed: --project internal, --project "big shop".
func flagText(flag, value string) string {
	if strings.ContainsAny(value, " \t\"'") {
		value = strconv.Quote(value)
	}
	return "--" + flag + " " + value
}

// sortedFlags is the flags of set, in the order of order.
func sortedFlags(set map[string]string, order []string) []string {
	var flags []string
	for _, flag := range order {
		if _, ok := set[flag]; ok {
			flags = append(flags, flag)
		}
	}
	return flags
}

// joinFlags is "--a, --b and --c" (conj "and" or "or").
func joinFlags(flags []string, conj string) string {
	dashed := make([]string, len(flags))
	for i, flag := range flags {
		dashed[i] = "--" + flag
	}
	if len(dashed) < 2 {
		return strings.Join(dashed, "")
	}
	return strings.Join(dashed[:len(dashed)-1], ", ") + " " + conj + " " + dashed[len(dashed)-1]
}

func printSteps(a *App, run record, out output.Table) error {
	steps, _ := run["steps"].([]any)
	if len(steps) == 0 {
		return nil
	}
	fmt.Fprintln(a.stdout, "\n"+out.Label("Steps:"))
	rows := make([][]string, 0, len(steps))
	var reasons []string
	for i, raw := range steps {
		step, _ := raw.(record)
		rows = append(rows, []string{
			strconv.Itoa(i + 1), output.Text(step["name"]), out.Cell("state", output.Text(step["state"])),
			output.LocalTime(step["started_at"]), output.LocalTime(step["updated_at"]),
		})
		if reason := output.Text(step["reason"]); reason != "-" {
			// A reason can span lines (a task message); keep them under their step.
			reason = strings.ReplaceAll(strings.TrimSpace(reason), "\n", "\n     ")
			if out.Color {
				reason = output.Paint(output.Red, reason)
			}
			reasons = append(reasons, fmt.Sprintf("  %d. %s\n     %s", i+1, output.Text(step["name"]), reason))
		}
	}
	if err := output.WriteTable(a.stdout, out.Headers([]string{"#", "STEP", "STATE", "STARTED", "UPDATED"}), rows); err != nil {
		return err
	}
	if len(reasons) > 0 {
		fmt.Fprintln(a.stdout, "\n"+out.Label("Why:"))
		fmt.Fprintln(a.stdout, strings.Join(reasons, "\n"))
	}
	return nil
}
