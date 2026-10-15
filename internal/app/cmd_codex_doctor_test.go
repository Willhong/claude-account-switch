package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexCredentialStoreModeReadsTopLevelSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# comment\ncli_auth_credentials_store = \"keyring\" # managed elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexCredentialStoreMode(path); got != "keyring" {
		t.Fatalf("mode = %q", got)
	}
}
