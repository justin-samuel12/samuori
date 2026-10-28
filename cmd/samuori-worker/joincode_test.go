package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/workerjoin"
)

func TestJoinCodeCommand(t *testing.T) {
	pin := "sha256:" + strings.Repeat("0f", 32)
	token := "samuoriw_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	code, err := workerjoin.Encode(workerjoin.Code{Controllers: []string{"127.0.0.1:1", "127.0.0.1:2"}, Pin: pin, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		n := joinCode(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
		return n, out.String(), errOut.String()
	}
	if n, out, _ := run(code+"\n", "--no-check"); n != 0 || out != "127.0.0.1:1,127.0.0.1:2\n"+pin+"\n"+token+"\n" {
		t.Fatalf("decode = %d %q", n, out)
	}
	if n, out, errOut := run("samuoric_abc\n", "--no-check"); n != 2 || out != "" || !strings.Contains(errOut, "malformed") {
		t.Fatalf("controller code = %d %q %q", n, out, errOut)
	}
	if n, _, _ := run(code, "--host", "x"); n != 2 {
		t.Fatalf("unknown argument = %d", n)
	}
	if n, out, errOut := run(code); n != 1 || out != "" || !strings.Contains(errOut, "no controller answers") {
		t.Fatalf("unreachable controllers = %d %q %q", n, out, errOut)
	}
}
