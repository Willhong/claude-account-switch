package keychain

import (
	"strings"
	"testing"
)

func TestSetStdinOnlyRejectsMultilineMetadataBeforeCallingSecurity(t *testing.T) {
	if err := SetStdinOnly("service", "account", "unsafe\nlabel", "value"); err == nil {
		t.Fatal("expected multiline Keychain metadata to be rejected")
	}
}

func TestSetStdinOnlyRejectsPayloadThatWouldExceedInteractiveLimit(t *testing.T) {
	if err := SetStdinOnly("service", "account", "label", strings.Repeat("x", stdinLimit)); err == nil {
		t.Fatal("expected an oversized interactive command to be rejected")
	}
}
