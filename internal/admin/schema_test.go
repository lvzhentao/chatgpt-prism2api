package admin

import "testing"

func TestConfigSchemaHasRestartFlags(t *testing.T) {
	schema := ConfigSchema()
	if len(schema) < 10 {
		t.Fatalf("schema too small: %d", len(schema))
	}
	var listen, allow bool
	for _, f := range schema {
		if f.Name == "listen_addr" {
			listen = f.RestartRequired
		}
		if f.Name == "allow_remote_admin" {
			allow = true
		}
	}
	if !listen {
		t.Fatal("listen_addr must be restart_required")
	}
	if !allow {
		t.Fatal("missing allow_remote_admin")
	}
}

func TestApplyAllowRemoteFromEnv(t *testing.T) {
	t.Setenv("WEB2API_ALLOW_REMOTE_ADMIN", "true")
	rc := &RuntimeConfig{}
	if !applyAllowRemoteFromEnv(rc) || !rc.AllowRemoteAdmin {
		t.Fatal("env true should enable")
	}
	if applyAllowRemoteFromEnv(rc) {
		t.Fatal("second apply should be no-op")
	}
	t.Setenv("WEB2API_ALLOW_REMOTE_ADMIN", "")
	rc2 := &RuntimeConfig{}
	if applyAllowRemoteFromEnv(rc2) || rc2.AllowRemoteAdmin {
		t.Fatal("empty env should leave default")
	}
}
