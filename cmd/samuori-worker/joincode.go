package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/workerjoin"
)

// joinCode is `samuori-worker join-code`, run by samuori-join before installing anything: it reads a worker join code on
// standard input, checks the pinned controller certificate unless --no-check, and prints the endpoints, the CA pin
// and the token on three lines.
func joinCode(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	check := true
	for _, a := range args {
		switch a {
		case "--no-check":
			check = false
		default:
			fmt.Fprintln(errOut, "usage: samuori-worker join-code [--no-check] < code")
			return 2
		}
	}
	line, err := bufio.NewReader(io.LimitReader(in, 8192)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(errOut, "samuori-worker join-code:", err)
		return 2
	}
	c, err := workerjoin.Decode(line)
	if err != nil {
		fmt.Fprintln(errOut, "samuori-worker join-code:", err)
		return 2
	}
	if check {
		if err := workerjoin.Check(ctx, c, 10*time.Second); err != nil {
			fmt.Fprintln(errOut, "samuori-worker join-code:", err)
			return 1
		}
	}
	fmt.Fprintf(out, "%s\n%s\n%s\n", strings.Join(c.Controllers, ","), c.Pin, c.Token)
	return 0
}
