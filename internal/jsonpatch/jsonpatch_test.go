package jsonpatch

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSetTopLevelKeyReplacesValueAndLeavesTheRestByteIdentical(t *testing.T) {
	doc := []byte(`{
  "numStartups": 4239,
  "oauthAccount": {
    "emailAddress": "old@example.com",
    "accountUuid": "aaa"
  },
  "projects": {
    "/tmp": {"allowedTools": []}
  }
}`)

	out, err := SetTopLevelKey(doc, "oauthAccount", []byte(`{"emailAddress": "new@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Fatalf("result is not valid JSON:\n%s", out)
	}
	if strings.Contains(string(out), "old@example.com") {
		t.Errorf("old value survived:\n%s", out)
	}
	if !strings.Contains(string(out), `"numStartups": 4239`) {
		t.Errorf("untouched key was reformatted:\n%s", out)
	}
	if !strings.Contains(string(out), `"/tmp": {"allowedTools": []}`) {
		t.Errorf("untouched nested value was reformatted:\n%s", out)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	acct := got["oauthAccount"].(map[string]any)
	if acct["emailAddress"] != "new@example.com" {
		t.Errorf("emailAddress = %v, want new@example.com", acct["emailAddress"])
	}
	if len(got) != 3 {
		t.Errorf("key count = %d, want 3", len(got))
	}
}

func TestSetTopLevelKeyInsertsMissingKey(t *testing.T) {
	out, err := SetTopLevelKey([]byte(`{"a": 1}`), "oauthAccount", []byte(`{"emailAddress":"x@y.z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Fatalf("result is not valid JSON:\n%s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["a"] != float64(1) {
		t.Errorf("existing key lost: %v", got)
	}
	if _, ok := got["oauthAccount"]; !ok {
		t.Errorf("key was not inserted: %s", out)
	}
}

func TestSetTopLevelKeyInsertsIntoEmptyObject(t *testing.T) {
	for _, doc := range []string{`{}`, "{\n}", "{   }"} {
		out, err := SetTopLevelKey([]byte(doc), "oauthAccount", []byte(`null`))
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if !json.Valid(out) {
			t.Errorf("%q produced invalid JSON: %s", doc, out)
		}
	}
}

func TestSetTopLevelKeyIgnoresNestedKeysOfTheSameName(t *testing.T) {
	doc := []byte(`{"projects":{"oauthAccount":"decoy"},"oauthAccount":"real"}`)
	out, err := SetTopLevelKey(doc, "oauthAccount", []byte(`"patched"`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got["oauthAccount"] != "patched" {
		t.Errorf("top-level value = %v, want patched", got["oauthAccount"])
	}
	if got["projects"].(map[string]any)["oauthAccount"] != "decoy" {
		t.Errorf("nested decoy was rewritten: %s", out)
	}
}

func TestSetTopLevelKeyRejectsNonObjects(t *testing.T) {
	if _, err := SetTopLevelKey([]byte(`[1,2,3]`), "k", []byte(`1`)); err == nil {
		t.Error("expected an error for a JSON array")
	}
}

func TestMarshalIndentNoEscapeKeepsAmpersandsIntact(t *testing.T) {
	out, err := MarshalIndentNoEscape(map[string]string{"name": "A & B <co>"}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Go escapes &, < and > by default; Claude Code's own writer does not, so
	// the value has to come back byte-for-byte, indented under the given prefix.
	want := "{\n    \"name\": \"A & B <co>\"\n  }"
	if string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}
