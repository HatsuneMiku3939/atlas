// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package migrate

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// stmts provides a generic implementation for extracting
// SQL statements from the given file string.
func stmts(input string) ([]string, error) {
	var stmts []string
	l, err := newLex(input)
	if err != nil {
		return nil, err
	}
	for {
		s, err := l.stmt()
		if err == io.EOF {
			return stmts, nil
		}
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, s)
	}
}

type lex struct {
	input string
	pos   int    // current position.
	width int    // size of latest rune.
	start int    // start position of current statement.
	depth int    // depth of parentheses.
	delim string // configured delimiter.
}

const (
	eos          = -1
	delimComment = "//atlas:delimiter "
)

func newLex(input string) (*lex, error) {
	delim := ";"
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, delimComment) {
		input = strings.TrimSpace(input[len(delimComment):])
		i := strings.Index(input, "\n")
		if i == -1 {
			return nil, errors.New("invalid delimiter")
		}
		delim = input[:i]
		input = input[i+1:]
	}
	l := &lex{input: input, delim: delim}
	return l, nil
}

func (l *lex) stmt() (string, error) {
	for {
		switch r := l.next(); {
		case r == eos:
			if l.depth > 0 {
				return "", errors.New("unclosed parentheses")
			}
			if l.pos > l.start {
				return l.input[l.start:], nil
			}
			return "", io.EOF
		case r == '(':
			l.depth++
		case r == ')':
			if l.depth == 0 {
				return "", fmt.Errorf("unexpected ')' at position %d", l.pos)
			}
			l.depth--
		case r == '\'', r == '"', r == '`':
			if err := l.skipQuote(r); err != nil {
				return "", err
			}
		case r == '-' && l.next() == '-':
			if i := strings.Index(l.input[l.pos:], "\n"); i != -1 {
				l.pos += i + 1
			}
		case r == '/' && l.next() == '*':
			i := strings.Index(l.input[l.pos:], "*/")
			if i < 0 {
				return "", fmt.Errorf("unclosed comment: %d", l.pos)
			}
			l.pos += i + 2
		case strings.HasPrefix(l.input[l.pos-l.width:], l.delim):
			if l.depth == 0 {
				stmt := l.input[l.start:l.pos]
				l.start = l.pos - l.width
				l.pos += len(l.delim)
				return stmt, nil
			}
		}
	}
}

func (l *lex) next() rune {
	if l.pos >= len(l.input) {
		return eos
	}
	r, w := utf8.DecodeRuneInString(l.input[l.pos:])
	l.pos += w
	l.width = w
	return r
}

func (l *lex) skipQuote(quote rune) error {
	for {
		switch r := l.next(); {
		case r == eos:
			return fmt.Errorf("unclosed quote %q", quote)
		case r == '\\':
			l.next()
		case r == quote:
			return nil
		}
	}
}
