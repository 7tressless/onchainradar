package config

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests exercise loadDotEnv in isolation. loadDotEnv mutates the process
// environment via os.Setenv (global state), so each test uses a unique OCR_DOTENV_TEST_
// key prefix and registers cleanup to avoid leaking into other tests. They are hermetic
// (temp file under t.TempDir, no network/DB/clock) and must not run in parallel, since
// they read and mutate the shared process environment.

const dotenvKeyPrefix = "OCR_DOTENV_TEST_"

// writeDotEnv writes content to a temporary .env file and returns its path.
func writeDotEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp .env: %v", err)
	}
	return path
}

// unsetAfter ensures key is removed from the process environment when the test
// finishes, so a key that loadDotEnv sets does not leak. It is used for keys
// whose file-loading we assert and therefore must not pre-exist (loadDotEnv
// skips keys already present, including those set to an empty string).
func unsetAfter(t *testing.T, key string) {
	t.Helper()
	t.Cleanup(func() {
		if err := os.Unsetenv(key); err != nil {
			t.Errorf("cleanup unset %s: %v", key, err)
		}
	})
}

// mustLookup fails the test if key is unset; otherwise returns its value.
func mustLookup(t *testing.T, key string) string {
	t.Helper()
	v, ok := os.LookupEnv(key)
	if !ok {
		t.Fatalf("expected %s to be set, but it is unset", key)
	}
	return v
}

func TestLoadDotEnv_NormalPairs(t *testing.T) {
	keyA := dotenvKeyPrefix + "NORMAL_A"
	keyB := dotenvKeyPrefix + "NORMAL_B"
	unsetAfter(t, keyA)
	unsetAfter(t, keyB)

	path := writeDotEnv(t, keyA+"=alpha\n"+keyB+"=beta\n")
	loadDotEnv(path)

	if got := mustLookup(t, keyA); got != "alpha" {
		t.Errorf("%s = %q, want %q", keyA, got, "alpha")
	}
	if got := mustLookup(t, keyB); got != "beta" {
		t.Errorf("%s = %q, want %q", keyB, got, "beta")
	}
}

func TestLoadDotEnv_TrimsSurroundingQuotes(t *testing.T) {
	dq := dotenvKeyPrefix + "DQUOTED"
	sq := dotenvKeyPrefix + "SQUOTED"
	unsetAfter(t, dq)
	unsetAfter(t, sq)

	path := writeDotEnv(t, dq+"=\"double\"\n"+sq+"='single'\n")
	loadDotEnv(path)

	if got := mustLookup(t, dq); got != "double" {
		t.Errorf("%s = %q, want %q (double quotes should be trimmed)", dq, got, "double")
	}
	if got := mustLookup(t, sq); got != "single" {
		t.Errorf("%s = %q, want %q (single quotes should be trimmed)", sq, got, "single")
	}
}

func TestLoadDotEnv_StripsLeadingBOM(t *testing.T) {
	key := dotenvKeyPrefix + "BOM_FIRST"
	unsetAfter(t, key)

	// A UTF-8 BOM (U+FEFF) prepended by a Windows editor must not corrupt the
	// first key. Without stripping, the key would parse as a U+FEFF-prefixed name
	// and never match a plain os.Getenv(key), silently falling back to defaults.
	// The BOM is written via its escape (a literal U+FEFF is illegal in Go source).
	content := "\uFEFF" + key + "=present\n"
	path := writeDotEnv(t, content)
	loadDotEnv(path)

	if got := mustLookup(t, key); got != "present" {
		t.Errorf("%s = %q, want %q (leading BOM should be stripped)", key, got, "present")
	}
	// The BOM-prefixed variant must not exist as its own key.
	if _, ok := os.LookupEnv("\uFEFF" + key); ok {
		t.Errorf("BOM was not stripped: a key %q leaked into the environment", "\uFEFF"+key)
	}
}

func TestLoadDotEnv_SkipsCommentLines(t *testing.T) {
	key := dotenvKeyPrefix + "AFTER_COMMENT"
	unsetAfter(t, key)

	// A commented-out assignment must not reach the environment; the following
	// real assignment must.
	path := writeDotEnv(t, "# "+key+"=commented\n"+key+"=real\n")
	loadDotEnv(path)

	if got := mustLookup(t, key); got != "real" {
		t.Errorf("%s = %q, want %q (comment line should be skipped)", key, got, "real")
	}
}

func TestLoadDotEnv_SkipsBlankLines(t *testing.T) {
	key := dotenvKeyPrefix + "AFTER_BLANK"
	unsetAfter(t, key)

	// Leading blank and whitespace-only lines must be ignored without error.
	path := writeDotEnv(t, "\n   \n\t\n"+key+"=value\n")
	loadDotEnv(path)

	if got := mustLookup(t, key); got != "value" {
		t.Errorf("%s = %q, want %q (blank lines should be skipped)", key, got, "value")
	}
}

func TestLoadDotEnv_SkipsMalformedLineWithNoKey(t *testing.T) {
	good := dotenvKeyPrefix + "GOOD"
	unsetAfter(t, good)

	// "noequalshere" has no '=' (IndexByte returns -1); "=orphanvalue" has an
	// empty key (IndexByte returns 0). Both satisfy eq <= 0 and are skipped.
	// The well-formed line after them must still load.
	path := writeDotEnv(t, "noequalshere\n=orphanvalue\n"+good+"=ok\n")
	loadDotEnv(path)

	if got := mustLookup(t, good); got != "ok" {
		t.Errorf("%s = %q, want %q (well-formed line after malformed ones should load)", good, got, "ok")
	}
	// The orphan value must not have created an empty-named env entry.
	if _, ok := os.LookupEnv(""); ok {
		t.Errorf("a malformed line created an env var with an empty key")
	}
}

func TestLoadDotEnv_DoesNotOverrideExistingVar(t *testing.T) {
	key := dotenvKeyPrefix + "PREEXISTING"
	// t.Setenv sets a real value and restores prior state on cleanup. Because
	// the key already exists, loadDotEnv must leave it untouched.
	t.Setenv(key, "from-environment")

	path := writeDotEnv(t, key+"=from-file\n")
	loadDotEnv(path)

	if got := mustLookup(t, key); got != "from-environment" {
		t.Errorf("%s = %q, want %q (existing env var must not be overridden)", key, got, "from-environment")
	}
}

func TestLoadDotEnv_MissingFileIsNoOp(t *testing.T) {
	// A non-existent path must be a silent no-op: no panic, no error surfaced,
	// and no spurious environment mutation. The defer recovers so a panic fails
	// the test with a clear message instead of aborting the run.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("loadDotEnv panicked on a missing file: %v", r)
		}
	}()

	missing := filepath.Join(t.TempDir(), "does-not-exist.env")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("precondition failed: expected %q to be absent, stat err = %v", missing, err)
	}

	loadDotEnv(missing) // must not panic
}
