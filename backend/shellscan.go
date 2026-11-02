package main

import (
	"errors"
	"fmt"
)

// scanShellCommand lexes one command line the way the remote shell family
// would, splitting it into segments — the simple commands of a pipeline or
// of a ;/&/newline list — with every word already unquoted. Quoting is what
// decides whether a byte is data or an operator, so it has to be modeled
// per shell family:
//
//   - POSIX shells (bash/zsh/fish/posix-other): single quotes protect
//     everything, double quotes protect everything but $( and `, an unquoted
//     backslash escapes the next byte (and vanishes from the word, as the
//     shell drops it), and an unquoted # starts a comment to end of line.
//   - cmd / PowerShell / unknown: only double quotes protect. cmd does not
//     know single quotes — 'a | del b' really runs del — and neither shell
//     treats backslash as an escape, so both stay ordinary bytes.
//
// Redirection, command substitution and unbalanced quoting are refused
// outright wherever the shell would honor them; everything else is handed
// back as words so the read-only gate can judge programs and flags on what
// the program would actually receive, never on raw bytes.
func scanShellCommand(command string, kind shellKind) ([][]string, error) {
	posix := kind == kindBash || kind == kindZsh || kind == kindFish || kind == kindPosixOther
	var segments [][]string
	var words []string
	word := make([]byte, 0, 32)
	inWord := false // true once a word started, even an empty "" one
	inSingle, inDouble, escaped, comment := false, false, false, false

	flushWord := func() {
		if inWord {
			words = append(words, string(word))
			word = word[:0]
			inWord = false
		}
	}
	endSegment := func() {
		flushWord()
		if len(words) > 0 {
			segments = append(segments, words)
			words = nil
		}
	}
	literal := func(c byte) {
		word = append(word, c)
		inWord = true
	}

	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case comment:
			if c == '\n' {
				comment = false
				endSegment()
			}
		case escaped:
			literal(c)
			escaped = false
		case inSingle:
			if c == '\'' {
				inSingle = false
			} else {
				literal(c)
			}
		case inDouble:
			switch c {
			case '"':
				inDouble = false
			case '`':
				return nil, errors.New("a backtick inside double quotes would run a command substitution")
			case '$':
				if i+1 < len(command) && command[i+1] == '(' {
					return nil, errors.New(`"$(" inside double quotes would run a command substitution`)
				}
				literal(c)
			default:
				literal(c)
			}
		default:
			switch c {
			case '"':
				inDouble, inWord = true, true
			case '\'':
				if posix {
					inSingle, inWord = true, true
				} else {
					literal(c)
				}
			case '\\':
				if posix {
					escaped = true
				} else {
					literal(c)
				}
			case '#':
				if posix && !inWord {
					comment = true
				} else {
					literal(c)
				}
			case '|', ';', '&', '\n':
				endSegment()
			case ' ', '\t', '\r':
				flushWord()
			case '<', '>':
				return nil, fmt.Errorf("%q outside quotes would redirect", string(c))
			case '`':
				return nil, errors.New("a backtick outside quotes would run a command substitution")
			case '$':
				if i+1 < len(command) && command[i+1] == '(' {
					return nil, errors.New(`"$(" outside quotes would run a command substitution`)
				}
				literal(c)
			default:
				literal(c)
			}
		}
	}
	switch {
	case inSingle || inDouble:
		return nil, errors.New("unterminated quote")
	case escaped:
		return nil, errors.New("trailing backslash")
	}
	endSegment()
	return segments, nil
}
