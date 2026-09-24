package proxypool

import (
	"testing"

	"prism-2api/internal/egress"
	"prism-2api/internal/persist"
)

func TestCreateListDefault(t *testing.T) {
	s, err := Open(persist.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Create(CreateRequest{
		Name:      "resin-main",
		Kind:      "resin",
		IsDefault: true,
		ResinURL:  "https://resin.example.com/tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != egress.KindResin {
		t.Fatalf("kind %s", p.Kind)
	}
	d, ok := s.Default()
	if !ok || d.ID != p.ID {
		t.Fatalf("default: %+v", d)
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list %d", len(list))
	}
}

func TestReload(t *testing.T) {
	mem := persist.NewMemory()
	s, _ := Open(mem)
	_, err := s.Create(CreateRequest{Name: "http-1", Kind: "http", HTTPProxy: "http://127.0.0.1:7890"})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(mem)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.List()) != 1 {
		t.Fatalf("reload %d", len(s2.List()))
	}
}
