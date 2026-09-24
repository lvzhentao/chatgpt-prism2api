package persist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ImportLegacyDir 把旧版凭据目录里的 JSON/YAML 读进 Backend。
// 只读不写回文件。目标已有同 kind/id 时跳过该条（不覆盖生产库）。
func ImportLegacyDir(b Backend, credDir string) (bool, error) {
	if b == nil || strings.TrimSpace(credDir) == "" {
		return false, nil
	}
	before := countDocs(b)
	if err := ImportConfigFile(b, filepath.Join(credDir, "config.json")); err != nil {
		return false, err
	}
	if err := ImportClientKeysFile(b, filepath.Join(credDir, "client_api_keys.json")); err != nil {
		return false, err
	}
	if err := ImportGroupsFile(b, filepath.Join(credDir, "groups.json")); err != nil {
		return false, err
	}
	if err := ImportAccountsDir(b, filepath.Join(credDir, "accounts")); err != nil {
		return false, err
	}
	return countDocs(b) > before, nil
}

// ImportConfigFile 导入 config.json，或同名 .yaml/.yml。已有 config/main 则跳过。
func ImportConfigFile(b Backend, path string) error {
	if b == nil || path == "" {
		return nil
	}
	if existing, err := b.LoadDoc(KindConfig, IDMain); err != nil {
		return err
	} else if len(bytes.TrimSpace(existing)) > 0 {
		return nil
	}
	candidates := append([]string{path}, yamlSiblings(path)...)
	data, err := readFirstExisting(candidates...)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return err
	}
	js, err := toJSON(data)
	if err != nil {
		return fmt.Errorf("import config %s: %w", path, err)
	}
	return b.SaveDoc(KindConfig, IDMain, js)
}

// ImportAccountsDir 导入 accounts/*.json（id = 文件名去后缀）。
func ImportAccountsDir(b Backend, dir string) error {
	if b == nil || dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if id == "" || id == "groups" {
			continue
		}
		if existing, err := b.LoadDoc(KindAccount, id); err != nil {
			return err
		} else if len(bytes.TrimSpace(existing)) > 0 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(bytes.TrimSpace(raw)) == 0 || !json.Valid(raw) {
			continue
		}
		if err := b.SaveDoc(KindAccount, id, raw); err != nil {
			return err
		}
	}
	return nil
}

// ImportClientKeysFile 导入 client_api_keys.json。
func ImportClientKeysFile(b Backend, path string) error {
	return importJSONBlob(b, KindClientKeys, IDAll, path)
}

// ImportGroupsFile 导入 groups.json。
func ImportGroupsFile(b Backend, path string) error {
	return importJSONBlob(b, KindGroups, IDAll, path)
}

func importJSONBlob(b Backend, kind, id, path string) error {
	if b == nil || path == "" {
		return nil
	}
	if existing, err := b.LoadDoc(kind, id); err != nil {
		return err
	} else if len(bytes.TrimSpace(existing)) > 0 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if !json.Valid(raw) {
		return fmt.Errorf("import %s: invalid json", path)
	}
	return b.SaveDoc(kind, id, raw)
}

func toJSON(data []byte) ([]byte, error) {
	t := bytes.TrimSpace(data)
	if len(t) == 0 {
		return nil, nil
	}
	if t[0] == '{' || t[0] == '[' {
		if !json.Valid(t) {
			return nil, fmt.Errorf("invalid json")
		}
		return t, nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func yamlSiblings(path string) []string {
	if strings.ToLower(filepath.Ext(path)) != ".json" {
		return nil
	}
	base := strings.TrimSuffix(path, filepath.Ext(path))
	return []string{base + ".yaml", base + ".yml"}
}

func readFirstExisting(paths ...string) ([]byte, error) {
	var last error
	for _, p := range paths {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		last = err
	}
	if last != nil {
		return nil, nil
	}
	return nil, nil
}

func countDocs(b Backend) int {
	n := 0
	for _, kind := range []string{KindConfig, KindAccount, KindClientKeys, KindGroups} {
		m, err := b.ListDocs(kind)
		if err != nil {
			continue
		}
		n += len(m)
	}
	return n
}
