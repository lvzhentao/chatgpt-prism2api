package pg

import (
	"os"
	"strings"
	"testing"
)

func TestOpenEmptyURL(t *testing.T) {
	db, err := Open(nil, "")
	if err != nil || db != nil {
		t.Fatalf("empty url: db=%v err=%v", db, err)
	}
}

func TestOpenIntegration(t *testing.T) {
	url := os.Getenv("WEB2API_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("set WEB2API_DATABASE_URL to run postgres integration")
	}
	db, err := Open(nil, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(nil); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentsRoundTrip(t *testing.T) {
	url := os.Getenv("WEB2API_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("set WEB2API_DATABASE_URL to run postgres integration")
	}
	db, err := Open(nil, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SaveDoc("account", "t-roundtrip", []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadDoc("account", "t-roundtrip")
	if err != nil || !strings.Contains(string(got), `"n"`) {
		t.Fatalf("load %q %v", got, err)
	}
	if err := db.DeleteDoc("account", "t-roundtrip"); err != nil {
		t.Fatal(err)
	}
	got, err = db.LoadDoc("account", "t-roundtrip")
	if err != nil || got != nil {
		t.Fatalf("deleted still there %q", got)
	}
}
