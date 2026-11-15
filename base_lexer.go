// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
)

var errDocumentStart = errors.New("already on document start")

// Budgets applied while unmarshalling untrusted session descriptions. Any
// standards-compliant description is orders of magnitude below these limits;
// they exist to bound the memory and CPU a malformed or hostile description
// can consume.
const (
	// maxLineLength bounds the bytes consumed by a single line.
	maxLineLength = 1 << 16
	// maxLines bounds the total number of lines in a description.
	maxLines = 1 << 15
	// maxAttributes bounds the total number of session and media
	// attributes in a description.
	maxAttributes = 1 << 14
)

type syntaxError struct {
	s string
	i int
}

func (e syntaxError) Error() string {
	if e.i < 0 {
		e.i = 0
	}

	return fmt.Sprintf("sdp: syntax error at pos %d: %s", e.i, strconv.QuoteToASCII(e.s[e.i:e.i+1]))
}

type baseLexer struct {
	value string
	pos   int
	// lineStart is the offset at which the line currently being consumed
	// started; numLines counts the lines consumed so far.
	lineStart int
	numLines  int
	// returned strings are copied here so that a retained one pins at most
	// one small buffer instead of the whole input
	copies strings.Builder
}

// line returns the 1-based number of the line currently being parsed.
// It is only called on error paths.
func (l *baseLexer) line() int {
	return strings.Count(l.value[:l.pos], "\n") + 1
}

// endLine records that the line ending at pos (just past its terminator)
// has been consumed and enforces the line length and line count budgets.
func (l *baseLexer) endLine(pos int) error {
	if pos-l.lineStart > maxLineLength {
		return fmt.Errorf("%w at line %d: %d bytes", errSDPLineTooLong, l.numLines+1, pos-l.lineStart)
	}
	l.lineStart = pos
	l.numLines++
	if l.numLines > maxLines {
		return fmt.Errorf("%w at line %d: limit is %d", errSDPTooManyLines, l.numLines, maxLines)
	}

	return nil
}

func (l *baseLexer) copyString(s string) string {
	const bufSize = 4096
	if l.copies.Cap()-l.copies.Len() < len(s) {
		l.copies = strings.Builder{}
		l.copies.Grow(max(min(bufSize, len(l.value)-l.pos+len(s)), len(s)))
	}
	l.copies.WriteString(s)
	all := l.copies.String()

	return all[len(all)-len(s):]
}

func (l *baseLexer) syntaxError() error {
	return syntaxError{s: l.value, i: l.pos - 1}
}

func (l *baseLexer) unreadByte() error {
	if l.pos <= 0 {
		return errDocumentStart
	}
	l.pos--

	return nil
}

func (l *baseLexer) readByte() (byte, error) {
	if l.pos >= len(l.value) {
		// Account for a final line that is not newline terminated.
		if l.pos > l.lineStart {
			if err := l.endLine(l.pos); err != nil {
				return byte(0), err
			}
		}

		return byte(0), io.EOF
	}
	ch := l.value[l.pos]
	l.pos++

	return ch, nil
}

func (l *baseLexer) nextLine() error {
	for {
		ch, err := l.readByte()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if ch == '\n' {
			if err := l.endLine(l.pos); err != nil {
				return err
			}

			continue
		}
		if ch == '\r' {
			continue
		}

		return l.unreadByte()
	}
}

func (l *baseLexer) readWhitespace() error { //notlint:cyclop
	for {
		ch, err := l.readByte()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if !isWhitespace(ch) {
			return l.unreadByte()
		}
	}
}

func (l *baseLexer) readUint64Field() (i uint64, err error) { //nolint:cyclop
	start := l.pos
	for {
		ch, err := l.readByte()
		if errors.Is(err, io.EOF) && i > 0 {
			break
		} else if err != nil {
			return i, err
		}

		if isNewline(ch) {
			if err := l.unreadByte(); err != nil {
				return i, err
			}

			break
		}

		if isWhitespace(ch) {
			if err := l.readWhitespace(); err != nil {
				return i, err
			}

			break
		}

		if ch < '0' || ch > '9' {
			return i, l.syntaxError()
		}
		if i > (math.MaxUint64-uint64(ch-'0'))/10 {
			return i, fmt.Errorf("%w at line %d: %s", errSDPNumericOverflow, l.line(), l.value[start:l.pos])
		}
		i = i*10 + uint64(ch-'0')
	}

	return i, nil
}

// Returns next field on this line or empty string if no more fields on line.
func (l *baseLexer) readField() (string, error) {
	start := l.pos
	var stop int
	for {
		stop = l.pos
		ch, err := l.readByte()
		if errors.Is(err, io.EOF) && stop > start {
			break
		} else if err != nil {
			return "", err
		}

		if isNewline(ch) {
			if err := l.unreadByte(); err != nil {
				return "", err
			}

			break
		}

		if isWhitespace(ch) {
			if err := l.readWhitespace(); err != nil {
				return "", err
			}

			break
		}
	}

	return l.copyString(l.value[start:stop]), nil
}

func (l *lexer) readRequiredField() (string, error) {
	field, err := l.readField()
	if err != nil {
		return "", err
	}

	if field == "" {
		return "", errFieldMissing
	}

	return field, nil
}

// Returns symbols until line end.
func (l *baseLexer) readLine() (string, error) {
	start := l.pos
	trim := 1
	for {
		ch, err := l.readByte()
		if err != nil {
			return "", err
		}
		if ch == '\r' {
			trim++
		}
		if ch == '\n' {
			if err := l.endLine(l.pos); err != nil {
				return "", err
			}

			return l.copyString(l.value[start : l.pos-trim]), nil
		}
	}
}

func (l *baseLexer) readType() (byte, error) {
	for {
		firstByte, err := l.readByte()
		if err != nil {
			return 0, err
		}

		if firstByte == '\r' {
			continue
		}
		if firstByte == '\n' {
			if err := l.endLine(l.pos); err != nil {
				return 0, err
			}

			continue
		}

		secondByte, err := l.readByte()
		if err != nil {
			return 0, err
		}

		if secondByte != '=' {
			return firstByte, l.syntaxError()
		}

		return firstByte, nil
	}
}

func isNewline(ch byte) bool { return ch == '\n' || ch == '\r' }

func isWhitespace(ch byte) bool { return ch == ' ' || ch == '\t' }

func anyOf(element string, data ...string) bool {
	return slices.Contains(data, element)
}
