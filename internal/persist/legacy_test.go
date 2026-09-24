package persist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportLegacyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"admin_username":"yadmin"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "groups.json"), []byte(`[{"name":"team"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	accDir := filepath.Join(dir, "accounts")
	if err := os.MkdirAll(accDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accDir, "alpha.json"), []byte(`{"api_key":"sk-x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	mem := NewMemory()
	ok, err := ImportLegacyDir(mem, dir)
	if err != nil || !ok {
		t.Fatalf("import ok=%v err=%v", ok, err)
	}
	cfg, _ := mem.LoadDoc(KindConfig, IDMain)
	if string(cfg) != `{"admin_username":"yadmin"}` {
		t.Fatalf("config %s", cfg)
	}
	acc, _ := mem.LoadDoc(KindAccount, "alpha")
	if string(acc) != `{"api_key":"sk-x"}` {
		t.Fatalf("account %s", acc)
	}

	// 不覆盖已有文档
	if err := os.WriteFile(filepath.Join(accDir, "alpha.json"), []byte(`{"api_key":"sk-new"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = ImportLegacyDir(mem, dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second import should not add docs")
	}
	acc, _ = mem.LoadDoc(KindAccount, "alpha")
	if string(acc) != `{"api_key":"sk-x"}` {
		t.Fatalf("must not overwrite: %s", acc)
	}
}

func TestImportConfigYAMLSibling(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("admin_username: yadmin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mem := NewMemory()
	if err := ImportConfigFile(mem, filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
	got, _ := mem.LoadDoc(KindConfig, IDMain)
	if !strings.Contains(string(got), `"admin_username"`) || !strings.Contains(string(got), `"yadmin"`) {
		t.Fatalf("yaml import %s", got)
	}
}
