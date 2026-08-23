// Package security contains small, dependency-free primitives shared by PUA
// and AgentHub when handling values that must never be written in clear text.
package security

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
)

const redactedValue = "<redacted>"

// Redactor replaces registered secret values in arbitrary text or bytes. It
// intentionally treats data as bytes: provider output can contain invalid
// UTF-8 and must still be scrubbed before it reaches a log or event sink.
type Redactor struct {
	mu       sync.RWMutex
	secrets  [][]byte
	escaped  [][]byte
	maxLen   int
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
	if len(b) > r.maxLen {
		r.maxLen = len(b)
	}
	// Structured provider events are JSON-encoded before the shared redactor
	// sees them. Register the escaped string form as well so values containing
	// quotes, controls, or HTML characters cannot bypass redaction merely by
	// crossing a JSON boundary.
	if encoded, err := json.Marshal(value); err == nil && len(encoded) >= 2 {
		escaped := encoded[1 : len(encoded)-1]
		if !bytes.Equal(escaped, b) && len(escaped) > 0 {
			duplicate := false
			for _, existing := range r.escaped {
				if bytes.Equal(existing, escaped) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				r.escaped = append(r.escaped, append([]byte(nil), escaped...))
			}
			if len(escaped) > r.maxLen {
				r.maxLen = len(escaped)
			}
		}
	}
	// Longer values first makes overlapping values deterministic and avoids
	// leaving a suffix of a longer secret exposed after a shorter match.
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
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
	r.mu.RLock()
	secrets := r.allSecretsLocked()
	r.mu.RUnlock()
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	result := append([]byte(nil), data...)
	for _, secret := range secrets {
		result = bytes.ReplaceAll(result, secret, []byte(redactedValue))
	}
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
	if len(s.pending) == 0 {
		return nil
	}
	keep := 0
	if s.redactor != nil {
		r := s.redactor
		r.mu.RLock()
		max := r.maxLen
		secrets := r.allSecretsLocked()
		r.mu.RUnlock()
		sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
		// First replace complete matches. Then retain the longest suffix that
		// is a prefix of any registered secret.
		for _, secret := range secrets {
			s.pending = bytes.ReplaceAll(s.pending, secret, []byte(redactedValue))
		}
		if !final && max > 1 {
			limit := max - 1
			if limit > len(s.pending) {
				limit = len(s.pending)
			}
			for n := limit; n > 0; n-- {
				suffix := s.pending[len(s.pending)-n:]
				for _, secret := range secrets {
					if len(secret) >= n && bytes.Equal(suffix, secret[:n]) {
						if n > keep {
							keep = n
						}
						break
					}
				}
				if keep == n {
					break
				}
			}
		}
	}
	if keep > len(s.pending) {
		keep = len(s.pending)
	}
	out := s.pending[:len(s.pending)-keep]
	if len(out) > 0 && s.dst != nil {
		if _, err := s.dst.Write(out); err != nil {
			return err
		}
	}
	if keep > 0 {
		s.pending = append([]byte(nil), s.pending[len(s.pending)-keep:]...)
	} else {
		s.pending = nil
	}
	return nil
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.allSecretsLocked() {
		if bytes.Contains(data, secret) {
			return true
		}
	}
	return false
}

func (r *Redactor) allSecretsLocked() [][]byte {
	result := make([][]byte, 0, len(r.secrets)+len(r.escaped))
	result = append(result, r.secrets...)
	result = append(result, r.escaped...)
	return result
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
