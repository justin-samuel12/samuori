package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
)

// Profiles live in ${XDG_CONFIG_HOME:-~/.config}/samuori: config.json holds URLs, tokens/NAME each token, all 0600.
// See docs/operators/cli.md#profiles.
type profile struct {
	URL    string `json:"url"`
	CAFile string `json:"caFile,omitempty"`
}

type config struct {
	Current  string             `json:"current,omitempty"`
	Profiles map[string]profile `json:"profiles"`
}

type settings struct {
	url, token, caFile string
}

var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func configDir(getenv func(string) string) (string, error) {
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "samuori"), nil
	}
	home := getenv("HOME")
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", errors.New("cannot find the home directory; set XDG_CONFIG_HOME")
		}
	}
	return filepath.Join(home, ".config", "samuori"), nil
}

func loadConfig(dir string) (config, error) {
	c := config{Profiles: map[string]profile{}}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("reading %s: %w", filepath.Join(dir, "config.json"), err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]profile{}
	}
	return c, nil
}

func saveConfig(dir string, c config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, "config.json"), append(raw, '\n'))
}

// writePrivate replaces path atomically with a 0600 file in a 0700 directory.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func tokenPath(dir, name string) string { return filepath.Join(dir, "tokens", name) }

// resolveSettings applies flags, then the environment (both already in g), then the selected profile. A profile's
// token and CA are used only for the profile's own URL, so its token never goes to a server named elsewhere.
func resolveSettings(g globalFlags, getenv func(string) string) (settings, error) {
	s := settings{url: g.url, caFile: g.caFile}
	if g.tokenFile != "" {
		token, err := readPrivate(g.tokenFile)
		if err != nil {
			return s, err
		}
		s.token = token
	} else {
		s.token = strings.TrimSpace(getenv("SAMUORI_TOKEN"))
	}
	if s.url != "" && s.token != "" && g.profile == "" {
		return s, nil
	}
	dir, err := configDir(getenv)
	if err != nil {
		return s, err
	}
	c, err := loadConfig(dir)
	if err != nil {
		return s, err
	}
	name := g.profile
	if name == "" {
		name = c.Current
	}
	if name == "" {
		return s, nil
	}
	p, ok := c.Profiles[name]
	if !ok {
		return s, fmt.Errorf("no profile %q; run samuori login --profile %s --url URL", name, name)
	}
	if s.url == "" {
		s.url = p.URL
	}
	if !sameURL(s.url, p.URL) {
		return s, nil
	}
	if s.caFile == "" {
		s.caFile = p.CAFile
	}
	if s.token == "" {
		token, err := readPrivate(tokenPath(dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return s, err
		}
		s.token = token
	}
	return s, nil
}

func sameURL(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

func (e *env) login(ctx context.Context, args []string) error {
	fs := flags("login", e)
	name := fs.String("profile", e.global.profile, "profile name")
	server := fs.String("url", e.global.url, "server URL")
	caFile := fs.String("ca-file", e.global.caFile, "PEM CA bundle")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	dir, err := configDir(e.getenv)
	if err != nil {
		return err
	}
	c, err := loadConfig(dir)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = "default"
	}
	if !profileName.MatchString(*name) {
		return failf(ExitUsage, "a profile name is lowercase letters, digits, - and _")
	}
	if old, ok := c.Profiles[*name]; ok {
		if *server == "" {
			*server = old.URL
		}
		if *caFile == "" {
			*caFile = old.CAFile
		}
	}
	if *server == "" {
		return failf(ExitUsage, "usage: samuori login [--profile NAME] --url URL [--ca-file FILE]")
	}
	if err := checkURL(*server); err != nil {
		return failf(ExitUsage, "%v", err)
	}
	if *caFile != "" {
		if *caFile, err = filepath.Abs(*caFile); err != nil {
			return err
		}
	}
	token, err := readToken(e.in, e.errOut)
	if err != nil {
		return failf(ExitUsage, "%v", err)
	}
	if err := e.connect(settings{url: *server, token: token, caFile: *caFile}); err != nil {
		return failf(ExitUsage, "%v", err)
	}
	me, err := e.api.GetMeWithResponse(ctx)
	if err != nil {
		return err
	}
	if me.JSON200 == nil {
		return problem("checking the token", me.StatusCode(), me.Body)
	}
	if err := writePrivate(tokenPath(dir, *name), []byte(token+"\n")); err != nil {
		return err
	}
	c.Profiles[*name] = profile{URL: strings.TrimRight(*server, "/"), CAFile: *caFile}
	c.Current = *name
	if err := saveConfig(dir, c); err != nil {
		return err
	}
	who := me.JSON200.Principal.Id
	if me.JSON200.DisplayName != nil {
		who = *me.JSON200.DisplayName
	}
	fmt.Fprintf(e.out, "Signed in to %s as %s; profile %q is the default.\n", *server, who, *name)
	return nil
}

// readToken reads one line from a pipe, or prompts on a terminal without echoing what is typed.
func readToken(in io.Reader, prompt io.Writer) (string, error) {
	if f, ok := in.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprint(prompt, "API token (input hidden): ")
			token, err := readHidden(f)
			fmt.Fprintln(prompt)
			if err != nil {
				return "", err
			}
			return checkToken(token)
		}
	}
	line, err := bufio.NewReader(io.LimitReader(in, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return checkToken(line)
}

func checkToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("no token given; pipe it on standard input or type it at the prompt")
	}
	return token, nil
}

func (e *env) logout(_ context.Context, args []string) error {
	fs := flags("logout", e)
	name := fs.String("profile", e.global.profile, "profile name")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	dir, err := configDir(e.getenv)
	if err != nil {
		return err
	}
	c, err := loadConfig(dir)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = c.Current
	}
	if _, ok := c.Profiles[*name]; !ok {
		return failf(ExitUsage, "no profile %q", *name)
	}
	if err := os.Remove(tokenPath(dir, *name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(c.Profiles, *name)
	if c.Current == *name {
		c.Current = ""
	}
	if err := saveConfig(dir, c); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "Removed profile %q and its stored token. Revoke the token in Account if it is no longer needed.\n", *name)
	return nil
}

func (e *env) profiles(_ context.Context, args []string) error {
	if _, err := parse(flags("profiles", e), args); err != nil {
		return err
	}
	dir, err := configDir(e.getenv)
	if err != nil {
		return err
	}
	c, err := loadConfig(dir)
	if err != nil {
		return err
	}
	type row struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		CAFile  string `json:"caFile,omitempty"`
		Default bool   `json:"default"`
		Token   bool   `json:"tokenStored"`
	}
	rows := []row{}
	for n, p := range c.Profiles {
		_, err := os.Stat(tokenPath(dir, n))
		rows = append(rows, row{n, p.URL, p.CAFile, n == c.Current, err == nil})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if e.json {
		return e.printJSON(rows)
	}
	if len(rows) == 0 {
		e.note("no profiles; run samuori login --url URL")
		return nil
	}
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tNAME\tURL\tTOKEN")
	for _, r := range rows {
		mark, tok := "", "missing"
		if r.Default {
			mark = "*"
		}
		if r.Token {
			tok = "stored"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, r.Name, r.URL, tok)
	}
	return tw.Flush()
}
