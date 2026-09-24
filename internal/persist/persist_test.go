package persist

import (
	"bytes"
	"testing"
)

func TestMemoryRoundTrip(t *testing.T) {
	m := NewMemory()
	if err := m.SaveDoc(KindAccount, "a", []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err := m.LoadDoc(KindAccount, "a")
	if err != nil || !bytes.Equal(got, []byte(`{"n":1}`)) {
		t.Fatalf("load %q %v", got, err)
	}
	list, err := m.ListDocs(KindAccount)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	if err := m.DeleteDoc(KindAccount, "a"); err != nil {
		t.Fatal(err)
	}
	got, err = m.LoadDoc(KindAccount, "a")
	if err != nil || got != nil {
		t.Fatalf("deleted still there %q", got)
	}
}
