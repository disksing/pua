package security

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestStreamRedactsSecretsAcrossChunksAndOnClose(t *testing.T) {
	var output bytes.Buffer
	redactor := NewRedactor("token-123", "secret-x")
	stream := redactor.NewStream(&output)
	if _, err := stream.Write([]byte("prefix tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("en-123 suffix secret-x")); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "prefix <redacted> suffix <redacted>" {
		t.Fatalf("redacted output = %q", got)
	}
	if bytes.Contains(output.Bytes(), []byte("token-123")) || bytes.Contains(output.Bytes(), []byte(" suffix secret-x")) {
		t.Fatalf("secret survived redaction: %q", output.String())
	}
}

func TestRedactorScrubsJSONEscapedSecret(t *testing.T) {
	redactor := NewRedactor("quote\"line\n")
	encoded, err := json.Marshal(map[string]string{"message": "quote\"line\n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(redactor.Redact(encoded)); bytes.Contains([]byte(got), []byte(`quote\"line`)) {
		t.Fatalf("JSON-escaped secret survived redaction: %s", got)
	}
}

func TestRedactorScrubsEquivalentJSONEscapes(t *testing.T) {
	redactor := NewRedactor("secret/😀\n")
	input := []byte(`{"message":"\u0073\u0065cret\/\uD83d\uDe00\u000A"}`)
	got := redactor.Redact(input)
	if redactor.ContainsSecret(got) {
		t.Fatalf("JSON-escaped secret survived redaction: %s", got)
	}
	var decoded map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("redacted output is invalid JSON: %v (%s)", err, got)
	}
	if decoded["message"] != redactedValue {
		t.Fatalf("redacted message = %q", decoded["message"])
	}
}

func TestStreamScrubsEquivalentJSONEscapesAcrossEverySplit(t *testing.T) {
	redactor := NewRedactor("secret/😀\n")
	input := []byte(`{"message":"\u0073ecret\/\uD83D\uDE00\n"}`)
	for split := 0; split <= len(input); split++ {
		t.Run(fmt.Sprintf("split-%d", split), func(t *testing.T) {
			got := streamRedact(t, redactor, input[:split], input[split:])
			if redactor.ContainsSecret(got) {
				t.Fatalf("JSON-escaped secret survived split %d: %s", split, got)
			}
			var decoded map[string]string
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("redacted output is invalid JSON: %v (%s)", err, got)
			}
			if decoded["message"] != redactedValue {
				t.Fatalf("redacted message = %q", decoded["message"])
			}
		})
	}
}

func TestStreamScrubsJSONEscapesWrittenOneByteAtATime(t *testing.T) {
	redactor := NewRedactor("secret/😀\n")
	input := []byte(`{"message":"\u0073ecret\/\uD83D\uDE00\n"}`)
	chunks := make([][]byte, len(input))
	for i := range input {
		chunks[i] = input[i : i+1]
	}
	got := streamRedact(t, redactor, chunks...)
	if redactor.ContainsSecret(got) {
		t.Fatalf("JSON-escaped secret survived byte chunks: %s", got)
	}
	var decoded map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("redacted output is invalid JSON: %v (%s)", err, got)
	}
	if decoded["message"] != redactedValue {
		t.Fatalf("redacted message = %q", decoded["message"])
	}
}

func TestStreamPrefersLongerOverlappingSecretAcrossChunks(t *testing.T) {
	redactor := NewRedactor("abc", "abcdef")
	got := streamRedact(t, redactor, []byte("abc"), []byte("def"))
	if string(got) != redactedValue {
		t.Fatalf("redacted output = %q", got)
	}
}

func TestRedactorNeverUsesMarkerContainingSecret(t *testing.T) {
	for _, secret := range []string{"<redacted>", "redacted", "act"} {
		t.Run(secret, func(t *testing.T) {
			redactor := NewRedactor(secret)
			got := redactor.Redact([]byte("prefix " + secret + " suffix"))
			if redactor.ContainsSecret(got) || bytes.Contains(got, []byte(secret)) {
				t.Fatalf("replacement exposed %q in %q", secret, got)
			}
		})
	}
}

func TestRedactorRemovesSecretCreatedAtReplacementBoundary(t *testing.T) {
	redactor := NewRedactor("red", "xy")
	got := redactor.Redact([]byte("xredy"))
	if len(got) != 0 {
		t.Fatalf("redacted output = %q", got)
	}
	if redactor.ContainsSecret(got) {
		t.Fatalf("replacement boundary exposed a secret: %q", got)
	}
}

func TestStreamRemovesSecretCreatedAcrossWrites(t *testing.T) {
	redactor := NewRedactor("red", "xy")
	got := streamRedact(t, redactor, []byte("xred"), []byte("y"))
	if len(got) != 0 {
		t.Fatalf("redacted output = %q", got)
	}
}

func TestRedactorHandlesBinaryData(t *testing.T) {
	redactor := NewRedactor("secret")
	input := []byte{0, 's', 'e', 'c', 'r', 'e', 't', 0xff}
	got := redactor.Redact(input)
	if !bytes.Equal(got, []byte{0, '<', 'r', 'e', 'd', 'a', 'c', 't', 'e', 'd', '>', 0xff}) {
		t.Fatalf("binary redaction = %v", got)
	}
}

func TestStreamHandlesBinarySecretAcrossEverySplit(t *testing.T) {
	redactor := NewRedactor("secret")
	input := []byte{0, 0xff, 's', 'e', 'c', 'r', 'e', 't', 0xfe}
	want := []byte{0, 0xff, '<', 'r', 'e', 'd', 'a', 'c', 't', 'e', 'd', '>', 0xfe}
	for split := 0; split <= len(input); split++ {
		t.Run(fmt.Sprintf("split-%d", split), func(t *testing.T) {
			got := streamRedact(t, redactor, input[:split], input[split:])
			if !bytes.Equal(got, want) {
				t.Fatalf("binary redaction = %v", got)
			}
		})
	}
}

func streamRedact(t *testing.T, redactor *Redactor, chunks ...[]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	stream := redactor.NewStream(&output)
	for _, chunk := range chunks {
		if _, err := stream.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
