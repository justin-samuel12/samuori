// Command samuori-web serves a built frontend and forwards API paths to the backend it starts (ADR 0055):
//
//	samuori-web [--listen :8080] [--root /srv/frontend] [--proxy /api]... [--backend http://127.0.0.1:8000] -- <backend command>
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"samuori.invalid/samuori/internal/version"
	"samuori.invalid/samuori/internal/webrun"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	fs := flag.NewFlagSet("samuori-web", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "address to listen on")
	root := fs.String("root", "/srv/frontend", "folder with the built frontend")
	backend := fs.String("backend", "http://127.0.0.1:8000", "where the backend listens")
	showVersion := fs.Bool("version", false, "print the version")
	var prefixes list
	fs.Var(&prefixes, "proxy", "path prefix forwarded to the backend (repeatable; default /api)")
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(version.Version)
		fmt.Println(version.Credit)
		return
	}
	target, err := url.Parse(*backend)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		fmt.Fprintln(os.Stderr, "samuori-web: --backend must be an http URL")
		os.Exit(2)
	}
	if len(prefixes) == 0 {
		prefixes = list{"/api"}
	}
	for _, p := range prefixes {
		if !strings.HasPrefix(p, "/") || p == "/" {
			fmt.Fprintf(os.Stderr, "samuori-web: --proxy %q must be a path such as /api\n", p)
			os.Exit(2)
		}
	}
	os.Exit(webrun.Run(context.Background(), webrun.Config{Listen: *listen, Root: *root, Prefixes: prefixes, Backend: target, Command: fs.Args()}, os.Stdout, os.Stderr))
}
