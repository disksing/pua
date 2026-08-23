package security

import (
	"bytes"
	"encoding/json"
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

func TestRedactorHandlesBinaryData(t *testing.T) {
	redactor := NewRedactor("secret")
	input := []byte{0, 's', 'e', 'c', 'r', 'e', 't', 0xff}
	got := redactor.Redact(input)
	if !bytes.Equal(got, []byte{0, '<', 'r', 'e', 'd', 'a', 'c', 't', 'e', 'd', '>', 0xff}) {
		t.Fatalf("binary redaction = %v", got)
	}
}
