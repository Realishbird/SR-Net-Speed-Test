package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteGnmicConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gnmic.yaml")
	cfg := DefaultConfig()
	cfg.Switches = []SwitchConfig{
		{Name: "snmp-only", Address: "10.0.0.3", SNMPCommunity: "public"},
		{Name: "z9864f", Address: "10.0.0.2", GNMIUser: "admin", GNMIPassword: `p"w`, GNMITLS: true},
	}
	n, err := WriteGnmicConfig(cfg, path)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	b, _ := os.ReadFile(path)
	y := string(b)
	for _, want := range []string{"address: 10.0.0.2:8080", `password: "p\"w"`, "skip-verify: true", "subscriptions: [counters, transceivers]", "encoding: json_ietf", "log: true", "http://127.0.0.1:8428/api/v1/write", "openconfig-interfaces"} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in:\n%s", want, y)
		}
	}
	if strings.Contains(y, "snmp-only") {
		t.Error("SNMP-only switch must not be a gNMI target")
	}
	cfg.Switches = nil
	if n, _ := WriteGnmicConfig(cfg, path); n != 0 {
		t.Error("expected 0 targets")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("config file should be removed when no gNMI switches remain")
	}
}
