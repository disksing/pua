// Package security contains small, dependency-free primitives shared by PUA
// and AgentHub when handling values that must never be written in clear text.
package security

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

const redactedValue = "<redacted>"

// Redactor replaces registered secret values in arbitrary text or bytes. It
// intentionally treats data as bytes: provider output can contain invalid
// UTF-8 and must still be scrubbed before it reaches a log or event sink.
type Redactor struct {
	mu      sync.RWMutex
	secrets [][]byte
}

// NewRedactor returns a redactor initialized with non-empty secret values.
// Empty values are ignored. Even short secrets are registered: a caller that
// explicitly marks a value secret must never have it reproduced in output.
func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	for _, value := range values {
		r.Register(value)
	}
	return r
}

// Register adds a secret value. Duplicate values are harmless. The value is
// copied so callers can safely reuse their input buffer.
func (r *Redactor) Register(value string) {
	if r == nil || value == "" {
		return
	}
	b := []byte(value)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.secrets {
		if bytes.Equal(existing, b) {
			return
		}
	}
	r.secrets = append(r.secrets, append([]byte(nil), b...))
}

// Secrets returns only lengths of the registered values. It is useful for
// diagnostics and tests without creating another API that can expose them.
func (r *Redactor) SecretLengths() []int {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]int, len(r.secrets))
	for i, secret := range r.secrets {
		result[i] = len(secret)
	}
	return result
}

// Redact replaces every registered secret in data and returns a new byte
// slice. The input is never modified.
func (r *Redactor) Redact(data []byte) []byte {
	if r == nil || len(data) == 0 {
		return append([]byte(nil), data...)
	}
	snapshot := r.snapshot()
	result, _, _ := snapshot.scan(data, true, snapshot.replacement)
	result, _ = snapshot.stabilize(result, true)
	return result
}

// RedactString is the string counterpart of Redact.
func (r *Redactor) RedactString(value string) string {
	return string(r.Redact([]byte(value)))
}

// NewStream creates a streaming writer. It retains the small suffix that can
// still be the prefix of a secret, allowing a secret split over arbitrary
// Write calls to be scrubbed without buffering an entire log.
func (r *Redactor) NewStream(dst io.Writer) *Stream {
	return &Stream{redactor: r, dst: dst}
}

// Stream is an io.WriteCloser that applies a Redactor before forwarding data.
type Stream struct {
	redactor *Redactor
	dst      io.Writer
	mu       sync.Mutex
	pending  []byte
	staged   []byte
	closed   bool
}

func (s *Stream) Write(p []byte) (int, error) {
	if s == nil {
		return 0, io.ErrClosedPipe
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	s.pending = append(s.pending, p...)
	return len(p), s.flush(false)
}

// Flush emits all data except a suffix that could be part of a future secret.
// It is safe to call between writes.
func (s *Stream) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flush(false)
}

func (s *Stream) flush(final bool) error {
	if len(s.pending) == 0 && len(s.staged) == 0 {
		return nil
	}
	if s.redactor == nil {
		if len(s.pending) > 0 {
			s.staged = append(s.staged, s.pending...)
			s.pending = nil
		}
		return s.writeStaged()
	}

	snapshot := s.redactor.snapshot()
	redacted, pending, _ := snapshot.scan(s.pending, final, snapshot.replacement)
	staged := append(append([]byte(nil), s.staged...), redacted...)
	out, staged := snapshot.stabilize(staged, final)
	if err := writeBytes(s.dst, out); err != nil {
		return err
	}
	s.pending = append(s.pending[:0], pending...)
	s.staged = append(s.staged[:0], staged...)
	return nil
}

func (s *Stream) writeStaged() error {
	if err := writeBytes(s.dst, s.staged); err != nil {
		return err
	}
	s.staged = nil
	return nil
}

func writeBytes(dst io.Writer, data []byte) error {
	if dst == nil || len(data) == 0 {
		return nil
	}
	n, err := dst.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// Close flushes the retained suffix and closes the destination when it also
// implements io.Closer. Closing a stream never writes a secret in clear text.
func (s *Stream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	err := s.flush(true)
	s.mu.Unlock()
	if closeErr, ok := s.dst.(io.Closer); ok {
		if err == nil {
			err = closeErr.Close()
		} else {
			_ = closeErr.Close()
		}
	}
	return err
}

// ContainsSecret reports whether any registered value occurs in data.
func (r *Redactor) ContainsSecret(data []byte) bool {
	if r == nil {
		return false
	}
	return r.snapshot().contains(data)
}

type redactionPattern struct {
	raw   []byte
	runes []rune
}

type redactionSnapshot struct {
	patterns    []redactionPattern
	replacement []byte
}

func (r *Redactor) snapshot() redactionSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot := redactionSnapshot{patterns: make([]redactionPattern, 0, len(r.secrets))}
	for _, secret := range r.secrets {
		pattern := redactionPattern{raw: append([]byte(nil), secret...)}
		if utf8.Valid(secret) {
			pattern.runes = []rune(string(secret))
		}
		snapshot.patterns = append(snapshot.patterns, pattern)
	}
	candidate := []byte(redactedValue)
	if !snapshot.contains(candidate) {
		snapshot.replacement = candidate
	}
	return snapshot
}

// scan applies leftmost-longest replacement. On a non-final scan it retains
// the first suffix that could become a match when more bytes arrive. Waiting
// even when a shorter match is already complete is what makes prefix-overlap
// safe (for example, "abc" and "abcdef").
func (s redactionSnapshot) scan(data []byte, final bool, replacement []byte) (out, pending []byte, changed bool) {
	if len(s.patterns) == 0 {
		return append([]byte(nil), data...), nil, false
	}
	out = make([]byte, 0, len(data))
	for pos := 0; pos < len(data); {
		matched, partial := s.matchAt(data[pos:])
		if partial && !final {
			return out, append([]byte(nil), data[pos:]...), changed
		}
		if matched > 0 {
			out = append(out, replacement...)
			pos += matched
			changed = true
			continue
		}
		out = append(out, data[pos])
		pos++
	}
	return out, nil, changed
}

// stabilize removes matches created where replacement or removal joined two
// formerly separate spans. Every changed pass gets shorter, so it terminates.
func (s redactionSnapshot) stabilize(data []byte, final bool) (out, pending []byte) {
	current := append([]byte(nil), data...)
	for {
		out, pending, changed := s.scan(current, final, nil)
		if !changed {
			return out, pending
		}
		current = append(out, pending...)
	}
}

func (s redactionSnapshot) contains(data []byte) bool {
	for pos := range data {
		matched, _ := s.matchAt(data[pos:])
		if matched > 0 {
			return true
		}
	}
	return false
}

func (s redactionSnapshot) matchAt(data []byte) (matched int, partial bool) {
	for _, pattern := range s.patterns {
		if len(data) >= len(pattern.raw) && bytes.Equal(data[:len(pattern.raw)], pattern.raw) {
			if len(pattern.raw) > matched {
				matched = len(pattern.raw)
			}
		} else if len(data) < len(pattern.raw) && bytes.Equal(data, pattern.raw[:len(data)]) {
			partial = true
		}

		if len(pattern.runes) > 0 {
			consumed, status := matchJSONRunes(data, pattern.runes)
			switch status {
			case jsonMatchComplete:
				if consumed > matched {
					matched = consumed
				}
			case jsonMatchPartial:
				partial = true
			}
		}
	}
	return matched, partial
}

type jsonMatchStatus uint8

const (
	jsonMatchNone jsonMatchStatus = iota
	jsonMatchPartial
	jsonMatchComplete
)

func matchJSONRunes(data []byte, target []rune) (int, jsonMatchStatus) {
	consumed := 0
	for _, want := range target {
		n, status := matchJSONRune(data[consumed:], want)
		if status != jsonMatchComplete {
			return 0, status
		}
		consumed += n
	}
	return consumed, jsonMatchComplete
}

func matchJSONRune(data []byte, target rune) (int, jsonMatchStatus) {
	if len(data) == 0 {
		return 0, jsonMatchPartial
	}
	if data[0] != '\\' {
		encoded := []byte(string(target))
		if len(data) < len(encoded) {
			if bytes.Equal(data, encoded[:len(data)]) {
				return 0, jsonMatchPartial
			}
			return 0, jsonMatchNone
		}
		if bytes.Equal(data[:len(encoded)], encoded) {
			return len(encoded), jsonMatchComplete
		}
		return 0, jsonMatchNone
	}
	if len(data) == 1 {
		return 0, jsonMatchPartial
	}
	if decoded, ok := shortJSONEscape(data[1]); ok {
		if decoded == target {
			return 2, jsonMatchComplete
		}
		return 0, jsonMatchNone
	}
	if data[1] != 'u' {
		return 0, jsonMatchNone
	}
	first, status := parseJSONCodeUnit(data)
	if status != jsonMatchComplete {
		return 0, status
	}
	if first < 0xd800 || first > 0xdfff {
		if rune(first) == target {
			return 6, jsonMatchComplete
		}
		return 0, jsonMatchNone
	}
	if first > 0xdbff {
		return 0, jsonMatchNone
	}
	second, status := parseJSONCodeUnit(data[6:])
	if status != jsonMatchComplete {
		return 0, status
	}
	if second < 0xdc00 || second > 0xdfff {
		return 0, jsonMatchNone
	}
	if utf16.DecodeRune(rune(first), rune(second)) == target {
		return 12, jsonMatchComplete
	}
	return 0, jsonMatchNone
}

func shortJSONEscape(value byte) (rune, bool) {
	switch value {
	case '"', '\\', '/':
		return rune(value), true
	case 'b':
		return '\b', true
	case 'f':
		return '\f', true
	case 'n':
		return '\n', true
	case 'r':
		return '\r', true
	case 't':
		return '\t', true
	default:
		return 0, false
	}
}

func parseJSONCodeUnit(data []byte) (uint16, jsonMatchStatus) {
	if len(data) == 0 {
		return 0, jsonMatchPartial
	}
	if data[0] != '\\' {
		return 0, jsonMatchNone
	}
	if len(data) == 1 {
		return 0, jsonMatchPartial
	}
	if data[1] != 'u' {
		return 0, jsonMatchNone
	}
	value := uint16(0)
	for i := 2; i < 6; i++ {
		if i >= len(data) {
			return 0, jsonMatchPartial
		}
		digit, ok := hexValue(data[i])
		if !ok {
			return 0, jsonMatchNone
		}
		value = value<<4 | uint16(digit)
	}
	return value, jsonMatchComplete
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

// NormalizeSecretValues removes duplicate/empty values and trims only the
// surrounding collection, never the secret itself. It is exported for callers
// that construct a redactor from a secret map.
func NormalizeSecretValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
