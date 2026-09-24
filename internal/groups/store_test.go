package groups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"prism-2api/internal/persist"
)

type fakeRenamer struct {
	old, new string
	n        int
}

func (f *fakeRenamer) RenameGroup(old, new string) {
	f.old, f.new = old, new
	f.n++
}

type fakeMember struct {
	groups []string
}

func (m *fakeMember) SetGroups(names []string) { m.groups = append([]string(nil), names...) }

func TestCreateThenListSorted(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("zz", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("aa", "first"); err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != 2 || list[0].Name != "aa" || list[0].Description != "first" || list[1].Name != "zz" {
		t.Fatalf("list = %+v", list)
	}
}

func TestCreateRejectsDuplicateEmptyLong(t *testing.T) {
	s, _ := Load("")
	if _, err := s.Create("dup", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("dup", ""); err == nil {
		t.Fatal("duplicate")
	}
	if _, err := s.Create("  dup  ", ""); err == nil {
		t.Fatal("trimmed duplicate")
	}
	if _, err := s.Create("", ""); err == nil {
		t.Fatal("empty")
	}
	if _, err := s.Create("   ", ""); err == nil {
		t.Fatal("blank")
	}
	if _, err := s.Create(strings.Repeat("a", 65), ""); err == nil {
		t.Fatal("too long")
	}
}

func TestMissingAndSetGroupsValidate(t *testing.T) {
	s, _ := Load("")
	if _, err := s.Create("known", ""); err != nil {
		t.Fatal(err)
	}
	miss := s.Missing([]string{"known", "ghost", "another-ghost", "ghost"})
	if len(miss) != 2 {
		t.Fatalf("missing = %v", miss)
	}
	m := &fakeMember{}
	if err := s.SetGroups(m, []string{"known", "ghost"}); err == nil {
		t.Fatal("expected unknown groups")
	}
	if err := s.SetGroups(m, []string{"known"}); err != nil {
		t.Fatal(err)
	}
	if len(m.groups) != 1 || m.groups[0] != "known" {
		t.Fatalf("groups = %v", m.groups)
	}
}

func TestUpdateConfigAndLimit(t *testing.T) {
	s, _ := Load("")
	if _, err := s.Create("g", "note"); err != nil {
		t.Fatal(err)
	}
	rpm, daily := s.Limit("g")
	if rpm != 0 || daily != 0 {
		t.Fatalf("empty overlay %d %d", rpm, daily)
	}
	desc := "updated"
	if _, err := s.Update("g", &desc, &Config{RPM: 12, DailyMax: 80}); err != nil {
		t.Fatal(err)
	}
	g, ok := s.Get("g")
	if !ok || g.Description != "updated" || g.Config == nil || g.Config.RPM != 12 || g.Config.DailyMax != 80 {
		t.Fatalf("updated = %+v", g)
	}
	rpm, daily = s.Limit("g")
	if rpm != 12 || daily != 80 {
		t.Fatalf("limit = %d %d", rpm, daily)
	}
	if _, err := s.Update("g", nil, &Config{}); err != nil {
		t.Fatal(err)
	}
	rpm, daily = s.Limit("g")
	if rpm != 0 || daily != 0 {
		t.Fatalf("cleared overlay %d %d", rpm, daily)
	}
}

func TestRenameCascadesAndConflicts(t *testing.T) {
	s, _ := Load("")
	if _, err := s.Create("old", "note"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("taken", ""); err != nil {
		t.Fatal(err)
	}
	acc := &fakeRenamer{}
	keys := &fakeRenamer{}
	s.SetAccountRenamer(acc)
	s.SetKeyRenamer(keys)

	if _, err := s.Rename("old", "taken"); err == nil {
		t.Fatal("conflict")
	}
	if !s.Exists("old") || !s.Exists("taken") {
		t.Fatal("conflict must not mutate")
	}
	if acc.n != 0 || keys.n != 0 {
		t.Fatal("no cascade on failed rename")
	}

	g, err := s.Rename("old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "new" || g.Description != "note" {
		t.Fatalf("renamed = %+v", g)
	}
	if s.Exists("old") || !s.Exists("new") {
		t.Fatal("rename key swap")
	}
	if acc.n != 1 || acc.old != "old" || acc.new != "new" {
		t.Fatalf("account cascade %+v", acc)
	}
	if keys.n != 1 || keys.old != "old" || keys.new != "new" {
		t.Fatalf("key cascade %+v", keys)
	}

	if _, err := s.Rename("new", "  new  "); err != nil {
		t.Fatal(err)
	}
	if acc.n != 1 {
		t.Fatal("same-name rename must not cascade")
	}
}

func TestDeleteAndRoundtrip(t *testing.T) {
	mem := persist.NewMemory()
	s, err := Open(mem)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("alpha", "a-desc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("beta", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update("beta", nil, &Config{RPM: 5}); err != nil {
		t.Fatal(err)
	}
	if !s.Delete("beta") || s.Delete("beta") {
		t.Fatal("delete flag")
	}

	s2, err := Open(mem)
	if err != nil {
		t.Fatal(err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].Name != "alpha" || list[0].Description != "a-desc" {
		t.Fatalf("reload = %+v", list)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")
	if err := os.WriteFile(path, []byte("  "), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("empty file")
	}
}

func TestDefaultPath(t *testing.T) {
	if got := DefaultPath("/cred"); got != filepath.Join("/cred", "groups.json") {
		t.Fatalf("DefaultPath = %s", got)
	}
}
