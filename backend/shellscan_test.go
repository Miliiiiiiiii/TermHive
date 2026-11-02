package main

import (
	"reflect"
	"strings"
	"testing"
)

// The scanner must hand back what each program would actually receive.
func TestScanShellCommandWords(t *testing.T) {
	cases := []struct {
		command string
		kind    shellKind
		want    [][]string
	}{
		{`ls -la /var/log`, kindBash, [][]string{{"ls", "-la", "/var/log"}}},
		{`cat "my file.txt" | sort | uniq -c`, kindBash,
			[][]string{{"cat", "my file.txt"}, {"sort"}, {"uniq", "-c"}}},
		{`systemctl is-active sshd && echo up || echo down`, kindBash,
			[][]string{{"systemctl", "is-active", "sshd"}, {"echo", "up"}, {"echo", "down"}}},
		// POSIX: single quotes and backslashes are quoting
		{`grep 'a|b' f`, kindBash, [][]string{{"grep", "a|b", "f"}}},
		{`grep a\|b f`, kindBash, [][]string{{"grep", "a|b", "f"}}},
		{`ls "" ''`, kindBash, [][]string{{"ls", "", ""}}},
		{"ls -l # not; rm -rf /", kindBash, [][]string{{"ls", "-l"}}},
		{"ls # c\nwc -l", kindBash, [][]string{{"ls"}, {"wc", "-l"}}},
		// double quotes keep backslashes as data in every family
		{`grep "a\|b" f`, kindBash, [][]string{{"grep", `a\|b`, "f"}}},
		{`grep "a\|b" f`, kindCmd, [][]string{{"grep", `a\|b`, "f"}}},
		// cmd: single quote and backslash are ordinary bytes
		{`dir C:\Users | findstr 'x'`, kindCmd, [][]string{{"dir", `C:\Users`}, {"findstr", "'x'"}}},
		{`grep 'a|b' f`, kindCmd, [][]string{{"grep", "'a"}, {"b'", "f"}}},
		{`tasklist |& findstr ssh`, kindUnknown, [][]string{{"tasklist"}, {"findstr", "ssh"}}},
	}
	for _, tc := range cases {
		got, err := scanShellCommand(tc.command, tc.kind)
		if err != nil {
			t.Errorf("%q (%s): unexpected error %v", tc.command, tc.kind, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q (%s):\n got %q\nwant %q", tc.command, tc.kind, got, tc.want)
		}
	}
}

// Quoted operators are data: the false rejections the old byte scan produced.
func TestReadOnlyAcceptsQuotedOperators(t *testing.T) {
	cases := []struct {
		command string
		kind    shellKind
	}{
		{`grep "a\|b" app.log`, kindUnknown},
		{`grep "^2026-11-02 13:\(0[5-9]\|1[0-9]\|2[0-9]\)" app.log | grep -c " ERROR "`, kindBash},
		{`grep "Spend Time ==> [0-9]* ms" app.log | tail -n 30`, kindUnknown},
		{`grep -v "==> [0-9] ms" app.log`, kindCmd},
		{`cat "a>b.txt" | grep "x<y"`, kindUnknown},
		{`grep 'a|b' app.log`, kindBash},
		{`grep 'a>b' app.log`, kindZsh},
		{`grep a\|b app.log`, kindPosixOther},
		{`grep "it's" app.log`, kindBash},
		{`echo "$HOME"`, kindBash},
		{`findstr "a|b" C:\log.txt`, kindCmd},
	}
	for _, tc := range cases {
		if err := isReadOnlyCommandFor(tc.command, tc.kind); err != nil {
			t.Errorf("expected read-only: %q (%s): %v", tc.command, tc.kind, err)
		}
	}
}

// Quoting must never hide what the program really receives (the flag
// denylist), nor what the shell still expands inside double quotes.
func TestReadOnlyRejectsThroughQuotes(t *testing.T) {
	cases := map[string]struct {
		command string
		kind    shellKind
	}{
		"quoted -delete reaches find":          {`find /tmp -name x "-delete"`, kindBash},
		"single-quoted -delete reaches find":   {`find /tmp -name x '-delete'`, kindBash},
		"quoted -f reaches tail":               {`tail "-f" /var/log/syslog`, kindUnknown},
		"$( expands inside double quotes":      {`echo "$(whoami)"`, kindBash},
		"backtick expands inside double quotes": {"echo \"`whoami`\"", kindBash},
		"$( outside quotes":                    {`echo $(whoami)`, kindCmd},
		"redirect outside quotes":              {`cat x > /tmp/y`, kindBash},
		"redirect after a quoted word":         {`cat "x" > /tmp/y`, kindBash},
		"cmd: single quotes do not protect |":  {`findstr 'a | del b' x`, kindCmd},
		"cmd: backslash does not escape |":     {`dir a\| del b`, kindCmd},
		"unknown: single quotes do not protect": {`grep 'a|rm -rf /tmp' x`, kindUnknown},
		"unterminated double quote":            {`echo "unclosed`, kindBash},
		"unterminated single quote":            {`echo 'unclosed`, kindBash},
		"trailing backslash":                   {`ls \`, kindBash},
		"chained program after quotes":         {`cat "a;b"; rm x`, kindBash},
	}
	for name, tc := range cases {
		if err := isReadOnlyCommandFor(tc.command, tc.kind); err == nil {
			t.Errorf("%s: expected rejection of %q (%s)", name, tc.command, tc.kind)
		}
	}
}

// The rejection reason must name the real problem, not a byte position.
func TestReadOnlyRejectionReasons(t *testing.T) {
	cases := []struct {
		command string
		kind    shellKind
		want    string
	}{
		{`echo "$(whoami)"`, kindBash, "inside double quotes"},
		{`cat x > y`, kindBash, "would redirect"},
		{`find x "-delete"`, kindBash, "would not be read-only"},
		{`echo "oops`, kindBash, "unterminated quote"},
		{`grep 'a|b' x`, kindCmd, "not on the read-only allowlist"},
	}
	for _, tc := range cases {
		err := isReadOnlyCommandFor(tc.command, tc.kind)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q (%s): want reason containing %q, got %v", tc.command, tc.kind, tc.want, err)
		}
	}
}
