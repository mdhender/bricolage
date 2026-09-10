// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// The structure commands (PLAN.md M7): sites, categories, output channels, and
// element types.
//
// earl is the acceptance-test harness for every milestone, not a debug toy: if
// earl cannot do it, the API is incomplete. Everything M7 adds is reachable
// from here, including the URI preview, which is the part a person most needs
// -- a URI format is configuration somebody types and gets wrong, and the only
// alternative to showing them what it produces is publishing something to find
// out.

// categoryResponse is a category as the API speaks it.
type categoryResponse struct {
	UID       string `json:"uid"`
	Site      int64  `json:"site"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Directory string `json:"directory"`
	Parent    string `json:"parent"`
	Depth     int    `json:"depth"`
}

// channelResponse is an output channel as the API speaks it.
type channelResponse struct {
	UID            string `json:"uid"`
	Site           int64  `json:"site"`
	Name           string `json:"name"`
	Protocol       string `json:"protocol"`
	Filename       string `json:"filename"`
	FileExt        string `json:"file_ext"`
	URIFormat      string `json:"uri_format"`
	FixedURIFormat string `json:"fixed_uri_format"`
	UseSlug        bool   `json:"use_slug"`
	URICase        string `json:"uri_case"`
}

// elementTypeResponse is an element type as the API speaks it.
type elementTypeResponse struct {
	UID       string `json:"uid"`
	KeyName   string `json:"key_name"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	TopLevel  bool   `json:"top_level"`
	FixedURI  bool   `json:"fixed_uri"`
	Paginated bool   `json:"paginated"`
	Schema    string `json:"schema"`
}

// siteResponse is a site as the API speaks it.
//
// The id is an integer and the uid is the name every route takes, which is the
// wart the API's own siteResponse describes: "site": 1 is what doc create has
// taken since M3. Both are printed, because the listing is where a person
// learns whichever one the command they are about to run wants.
type siteResponse struct {
	ID     int64  `json:"id"`
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	Active bool   `json:"active"`
}

// newSiteCmd is the parent of the site commands.
func newSiteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "List the sites and change what they are called",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newSiteListCmd(), newSiteUpdateCmd())
	return cmd
}

// newSiteListCmd lists the sites, which is how a person learns the id every
// other command wants and the uid "site update" wants.
func newSiteListCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the sites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out struct {
				Sites []siteResponse `json:"sites"`
			}
			if err := client.Get(cmd.Context(), "/api/v1/sites", &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tUID\tDOMAIN\tNAME\tACTIVE")
			for _, s := range out.Sites {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%t\n", s.ID, s.UID, s.Domain, s.Name, s.Active)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// newSiteUpdateCmd renames a site (issue #3).
//
// The long help says what the command cannot do, because the half it cannot do
// is the half that breaks a live system: the domain is the first path segment
// of the template tree, and renaming the site does not rename the directory.
func newSiteUpdateCmd() *cobra.Command {
	var (
		server     string
		asJSON     bool
		name       string
		domainName string
	)
	cmd := &cobra.Command{
		Use:   "update UID",
		Short: "Change a site's domain or its display name",
		Long: "Change a site's domain or its display name.\n\n" +
			"The domain is the host every URL of this site is built on and the first\n" +
			"path segment of the template tree:\n\n" +
			"    <templates>/<site domain>/<category path>/<element type>.gohtml\n\n" +
			"so changing it moves where every one of this site's templates is looked\n" +
			"for. Rename that directory at the same time; nothing in this system\n" +
			"creates or moves a directory in the template tree.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			body := map[string]any{}
			if cmd.Flags().Changed("domain") {
				body["domain"] = domainName
			}
			if cmd.Flags().Changed("name") {
				body["name"] = name
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: give --domain, --name, or both")
			}
			var site siteResponse
			if err := client.Do(cmd.Context(), http.MethodPatch,
				"/api/v1/sites/"+url.PathEscape(args[0]), body, &site); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, site)
			}
			fmt.Fprintf(w, "updated: %s\n", site.Domain)
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "uid\t%s\n", site.UID)
			fmt.Fprintf(tw, "id\t%d\n", site.ID)
			fmt.Fprintf(tw, "domain\t%s\n", site.Domain)
			fmt.Fprintf(tw, "name\t%s\n", site.Name)
			fmt.Fprintf(tw, "active\t%t\n", site.Active)
			fmt.Fprintf(tw, "templates\t%s/\n", site.Domain)
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&domainName, "domain", "", "the host this site publishes on")
	cmd.Flags().StringVar(&name, "name", "", "the site's display name")
	return cmd
}

// newCategoryCmd is the parent of the category commands.
func newCategoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "category",
		Aliases: []string{"cat"},
		Short:   "Create, move, and inspect categories",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newCategoryListCmd(),
		newCategoryCreateCmd(),
		newCategoryShowCmd(),
		newCategoryMoveCmd(),
		newCategoryDeleteCmd(),
	)
	return cmd
}

func newCategoryListCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		site   int64
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List a site's categories, in path order",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out struct {
				Categories []categoryResponse `json:"categories"`
			}
			if err := client.Get(cmd.Context(),
				fmt.Sprintf("/api/v1/categories?site=%d", site), &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			// Indented by depth, which is what the path order is for: the
			// listing reads as a tree without the client sorting it.
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PATH\tNAME\tUID")
			for _, c := range out.Categories {
				fmt.Fprintf(tw, "%s%s\t%s\t%s\n", strings.Repeat("  ", c.Depth), c.Path, c.Name, c.UID)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().Int64Var(&site, "site", 1, "the site whose categories to list")
	return cmd
}

func newCategoryCreateCmd() *cobra.Command {
	var (
		server    string
		asJSON    bool
		site      int64
		parent    string
		directory string
		name      string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a category under its parent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			if name == "" {
				name = directory
			}
			body := map[string]any{
				"site": site, "parent": parent, "directory": directory, "name": name,
			}
			var c categoryResponse
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/categories", body, &c); err != nil {
				return err
			}
			return printCategory(cmd.OutOrStdout(), c, asJSON, "created")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().Int64Var(&site, "site", 1, "the site the category belongs to")
	cmd.Flags().StringVar(&parent, "parent", "/", "the parent's path, '/'-terminated")
	cmd.Flags().StringVar(&directory, "directory", "", "the single path segment this category adds")
	cmd.Flags().StringVar(&name, "name", "", "the display name; defaults to the directory")
	_ = cmd.MarkFlagRequired("directory")
	return cmd
}

func newCategoryShowCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "show UID",
		Short: "Show one category",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var c categoryResponse
			if err := client.Get(cmd.Context(), categoryPath(args[0]), &c); err != nil {
				return err
			}
			return printCategory(cmd.OutOrStdout(), c, asJSON, "")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// newCategoryMoveCmd moves or renames a category, which rewrites the path of
// every descendant in one statement (PLAN.md M7 acceptance 1).
func newCategoryMoveCmd() *cobra.Command {
	var (
		server    string
		asJSON    bool
		parent    string
		directory string
	)
	cmd := &cobra.Command{
		Use:   "move UID",
		Short: "Move or rename a category, rewriting every descendant's path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			// Read through Changed rather than compared to a zero value: a
			// move and a rename are different requests and both are optional,
			// so "not mentioned" has to be distinguishable from "empty".
			body := map[string]any{}
			if cmd.Flags().Changed("parent") {
				body["parent"] = parent
			}
			if cmd.Flags().Changed("directory") {
				body["directory"] = directory
			}
			if len(body) == 0 {
				return fmt.Errorf("give --parent, --directory, or both; there is nothing to move otherwise")
			}
			var c categoryResponse
			if err := client.Do(cmd.Context(), http.MethodPatch, categoryPath(args[0]), body, &c); err != nil {
				return err
			}
			return printCategory(cmd.OutOrStdout(), c, asJSON, "moved")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&parent, "parent", "", "the new parent's path, '/'-terminated")
	cmd.Flags().StringVar(&directory, "directory", "", "the new path segment")
	return cmd
}

func newCategoryDeleteCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "delete UID",
		Short: "Delete a category that has no children and no documents",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			if err := client.Do(cmd.Context(), http.MethodDelete, categoryPath(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted: %s\n", args[0])
			return nil
		},
	}
	addServerFlag(cmd, &server)
	return cmd
}

func categoryPath(uid string) string { return "/api/v1/categories/" + url.PathEscape(uid) }

func printCategory(w io.Writer, c categoryResponse, asJSON bool, verb string) error {
	if asJSON {
		return writeJSON(w, c)
	}
	if verb != "" {
		fmt.Fprintf(w, "%s: %s\n", verb, c.Path)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "uid\t%s\n", c.UID)
	fmt.Fprintf(tw, "path\t%s\n", c.Path)
	fmt.Fprintf(tw, "name\t%s\n", c.Name)
	fmt.Fprintf(tw, "site\t%d\n", c.Site)
	if c.Parent != "" {
		fmt.Fprintf(tw, "parent\t%s\n", c.Parent)
	} else {
		fmt.Fprintf(tw, "parent\t(this is the site's root)\n")
	}
	return tw.Flush()
}

// newChannelCmd is the parent of the output channel commands.
func newChannelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "output-channel",
		Aliases: []string{"oc"},
		Short:   "Configure where content goes and what its address looks like",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newChannelListCmd(), newChannelCreateCmd(), newChannelUpdateCmd())
	return cmd
}

func newChannelListCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		site   int64
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the output channels",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			path := "/api/v1/output-channels"
			if cmd.Flags().Changed("site") {
				path += fmt.Sprintf("?site=%d", site)
			}
			var out struct {
				Channels []channelResponse `json:"output_channels"`
			}
			if err := client.Get(cmd.Context(), path, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSITE\tURI FORMAT\tFIXED\tSLUG\tCASE\tUID")
			for _, c := range out.Channels {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%t\t%s\t%s\n",
					c.Name, c.Site, c.URIFormat, c.FixedURIFormat, c.UseSlug, c.URICase, c.UID)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().Int64Var(&site, "site", 0, "restrict to one site")
	return cmd
}

// channelFlags are the fields both create and update take.
type channelFlags struct {
	name     string
	protocol string
	filename string
	fileExt  string
	format   string
	fixed    string
	useSlug  bool
	uriCase  string
}

func addChannelFlags(cmd *cobra.Command, f *channelFlags) {
	cmd.Flags().StringVar(&f.name, "name", "", "the channel's name, unique on its site")
	cmd.Flags().StringVar(&f.protocol, "protocol", "", `the URL scheme, such as "https://"`)
	cmd.Flags().StringVar(&f.filename, "filename", "", `the file written in each URI's directory (default "index")`)
	cmd.Flags().StringVar(&f.fileExt, "file-ext", "", `the file's extension (default "html")`)
	cmd.Flags().StringVar(&f.format, "uri-format", "",
		"the strftime URI format, with %{categories} and %{slug}")
	cmd.Flags().StringVar(&f.fixed, "fixed-uri-format", "",
		"the format used for element types that declare a fixed URI")
	cmd.Flags().BoolVar(&f.useSlug, "use-slug", false, "expand %{slug}")
	cmd.Flags().StringVar(&f.uriCase, "uri-case", "", "mixed, lower, or upper")
}

func (f channelFlags) body(cmd *cobra.Command) map[string]any {
	body := map[string]any{}
	for name, v := range map[string]string{
		"name":             f.name,
		"protocol":         f.protocol,
		"filename":         f.filename,
		"file_ext":         f.fileExt,
		"uri_format":       f.format,
		"fixed_uri_format": f.fixed,
		"uri_case":         f.uriCase,
	} {
		if v != "" {
			body[name] = v
		}
	}
	if cmd.Flags().Changed("use-slug") {
		body["use_slug"] = f.useSlug
	}
	return body
}

func newChannelCreateCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		site   int64
		f      channelFlags
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an output channel",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			body := f.body(cmd)
			body["site"] = site
			var c channelResponse
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/output-channels", body, &c); err != nil {
				return err
			}
			return printChannel(cmd.OutOrStdout(), c, asJSON, "created")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().Int64Var(&site, "site", 1, "the site the channel belongs to")
	addChannelFlags(cmd, &f)
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// newChannelUpdateCmd replaces a channel's configuration.
//
// It is a replacement rather than a patch, which is why every flag defaults to
// the server's default rather than to the channel's current value: an omitted
// URI format means "the ordinary one", not "leave it". That is a sharper edge
// than a patch would have, and it is the honest shape of what the route does.
func newChannelUpdateCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		f      channelFlags
	)
	cmd := &cobra.Command{
		Use:   "update UID",
		Short: "Replace an output channel's configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var c channelResponse
			if err := client.Do(cmd.Context(), http.MethodPatch,
				"/api/v1/output-channels/"+url.PathEscape(args[0]), f.body(cmd), &c); err != nil {
				return err
			}
			return printChannel(cmd.OutOrStdout(), c, asJSON, "updated")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	addChannelFlags(cmd, &f)
	return cmd
}

func printChannel(w io.Writer, c channelResponse, asJSON bool, verb string) error {
	if asJSON {
		return writeJSON(w, c)
	}
	if verb != "" {
		fmt.Fprintf(w, "%s: %s\n", verb, c.Name)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "uid\t%s\n", c.UID)
	fmt.Fprintf(tw, "name\t%s\n", c.Name)
	fmt.Fprintf(tw, "site\t%d\n", c.Site)
	fmt.Fprintf(tw, "uri format\t%s\n", c.URIFormat)
	fmt.Fprintf(tw, "fixed uri format\t%s\n", c.FixedURIFormat)
	fmt.Fprintf(tw, "use slug\t%t\n", c.UseSlug)
	fmt.Fprintf(tw, "uri case\t%s\n", c.URICase)
	fmt.Fprintf(tw, "file\t%s.%s\n", c.Filename, c.FileExt)
	fmt.Fprintf(tw, "protocol\t%s\n", c.Protocol)
	return tw.Flush()
}

// newElementTypeCmd is the parent of the element type commands.
func newElementTypeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "element-type",
		Aliases: []string{"et"},
		Short:   "Declare what fields a document carries",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newElementTypeListCmd(), newElementTypeCreateCmd(), newElementTypeUpdateCmd())
	return cmd
}

func newElementTypeListCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the element types",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out struct {
				ElementTypes []elementTypeResponse `json:"element_types"`
			}
			if err := client.Get(cmd.Context(), "/api/v1/element-types", &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tNAME\tKIND\tFIXED URI\tSCHEMA")
			for _, et := range out.ElementTypes {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", et.KeyName, et.Name, et.Kind, et.FixedURI, et.Schema)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// readSchema resolves --schema and --schema-file, the way readContent resolves
// the content flags: a file named "-" is standard input, so an agent can pipe
// a schema in without a temporary file.
func readSchema(cmd *cobra.Command, schema, file string) (string, bool, error) {
	switch {
	case cmd.Flags().Changed("schema") && cmd.Flags().Changed("schema-file"):
		return "", false, fmt.Errorf("--schema and --schema-file are two ways to say the same thing; give one")
	case cmd.Flags().Changed("schema"):
		return schema, true, nil
	case cmd.Flags().Changed("schema-file"):
		if file == "-" {
			b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			if err != nil {
				return "", false, fmt.Errorf("reading the schema from stdin: %w", err)
			}
			return string(b), true, nil
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return "", false, fmt.Errorf("reading %s: %w", file, err)
		}
		return string(b), true, nil
	default:
		return "", false, nil
	}
}

func addSchemaFlags(cmd *cobra.Command, schema, file *string) {
	cmd.Flags().StringVar(schema, "schema", "", `the JSON field declaration, such as {"fields":[{"name":"body","type":"block"}]}`)
	cmd.Flags().StringVar(file, "schema-file", "", `read the schema from a file, or "-" for stdin`)
}

func newElementTypeCreateCmd() *cobra.Command {
	var (
		server     string
		asJSON     bool
		keyName    string
		name       string
		kind       string
		topLevel   bool
		fixedURI   bool
		paginated  bool
		schema     string
		schemaFile string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an element type",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			body, given, err := readSchema(cmd, schema, schemaFile)
			if err != nil {
				return err
			}
			req := map[string]any{
				"key_name": keyName, "kind": kind,
				"top_level": topLevel, "fixed_uri": fixedURI, "paginated": paginated,
			}
			if name != "" {
				req["name"] = name
			}
			if given {
				req["schema"] = body
			}
			var et elementTypeResponse
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/element-types", req, &et); err != nil {
				return err
			}
			return printElementType(cmd.OutOrStdout(), et, asJSON, "created")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&keyName, "key-name", "", "the external identifier a document names its type by")
	cmd.Flags().StringVar(&name, "name", "", "the display name; defaults to the key name")
	cmd.Flags().StringVar(&kind, "kind", "story", "story, media, or template")
	cmd.Flags().BoolVar(&topLevel, "top-level", true, "documents of this type stand on their own")
	cmd.Flags().BoolVar(&fixedURI, "fixed-uri", false, "use the channel's fixed URI format, which carries no date")
	cmd.Flags().BoolVar(&paginated, "paginated", false, "documents of this type are paginated")
	addSchemaFlags(cmd, &schema, &schemaFile)
	_ = cmd.MarkFlagRequired("key-name")
	return cmd
}

func newElementTypeUpdateCmd() *cobra.Command {
	var (
		server     string
		asJSON     bool
		name       string
		topLevel   bool
		fixedURI   bool
		paginated  bool
		schema     string
		schemaFile string
	)
	cmd := &cobra.Command{
		Use:   "update KEY",
		Short: "Change an element type's name, flags, or schema",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			body, given, err := readSchema(cmd, schema, schemaFile)
			if err != nil {
				return err
			}
			// Only what was asked for is sent: an omitted flag leaves the
			// column alone, which is what a PATCH means and what makes
			// "turn fixed_uri off" distinguishable from "do not mention it".
			req := map[string]any{}
			if cmd.Flags().Changed("name") {
				req["name"] = name
			}
			if cmd.Flags().Changed("top-level") {
				req["top_level"] = topLevel
			}
			if cmd.Flags().Changed("fixed-uri") {
				req["fixed_uri"] = fixedURI
			}
			if cmd.Flags().Changed("paginated") {
				req["paginated"] = paginated
			}
			if given {
				req["schema"] = body
			}
			if len(req) == 0 {
				return fmt.Errorf("nothing to change; give --name, --top-level, --fixed-uri, --paginated, or a schema")
			}

			var et elementTypeResponse
			if err := client.Do(cmd.Context(), http.MethodPatch,
				"/api/v1/element-types/"+url.PathEscape(args[0]), req, &et); err != nil {
				return err
			}
			return printElementType(cmd.OutOrStdout(), et, asJSON, "updated")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&name, "name", "", "the display name")
	cmd.Flags().BoolVar(&topLevel, "top-level", true, "documents of this type stand on their own")
	cmd.Flags().BoolVar(&fixedURI, "fixed-uri", false, "use the channel's fixed URI format, which carries no date")
	cmd.Flags().BoolVar(&paginated, "paginated", false, "documents of this type are paginated")
	addSchemaFlags(cmd, &schema, &schemaFile)
	return cmd
}

func printElementType(w io.Writer, et elementTypeResponse, asJSON bool, verb string) error {
	if asJSON {
		return writeJSON(w, et)
	}
	if verb != "" {
		fmt.Fprintf(w, "%s: %s\n", verb, et.KeyName)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "key name\t%s\n", et.KeyName)
	fmt.Fprintf(tw, "name\t%s\n", et.Name)
	fmt.Fprintf(tw, "kind\t%s\n", et.Kind)
	fmt.Fprintf(tw, "top level\t%t\n", et.TopLevel)
	fmt.Fprintf(tw, "fixed uri\t%t\n", et.FixedURI)
	fmt.Fprintf(tw, "paginated\t%t\n", et.Paginated)
	fmt.Fprintf(tw, "schema\t%s\n", et.Schema)
	return tw.Flush()
}
