package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteGnmicConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gnmic.yaml")
	cfg := DefaultConfig()
	cfg.Switches = []SwitchConfig{
		{Name: "snmp-only", Address: "10.0.0.3", SNMPCommunity: "public"},
		{Name: "z9864f", Address: "10.0.0.2", GNMIUser: "admin", GNMIPassword: `p"w`, GNMITLS: true},
	}
	n, err := WriteGnmicConfig(cfg, path, map[string][]string{"z9864f": {"Ethernet8", "Ethernet0", "Ethernet0", "bad name; rm -rf", "PortChannel1"}})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	b, _ := os.ReadFile(path)
	y := string(b)
	for _, want := range []string{"address: 10.0.0.2:8080", `password: "p\"w"`, "skip-verify: true", `subscriptions: [counters, "transceivers-z9864f", "qos-z9864f", "health-z9864f"]`, "interface[interface-id=Ethernet0]/input/priority-groups", "ethernet/state/counters", "/openconfig-system:system/cpus", "interface[interface-id=Ethernet0]/output/queues", "interface[interface-id=Ethernet8]/pfc", "ethernet/state/port-speed", "encoding: json_ietf", "log: true", "sample-interval: 30s", "sample-interval: 60s", "http://127.0.0.1:8428/api/v1/write", "openconfig-interfaces",
		"component[name=Ethernet0]/transceiver/state\n      - /openconfig-platform:components/component[name=Ethernet0]/state/temperature\n      - /openconfig-platform:components/component[name=Ethernet8]/transceiver/state"} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in:\n%s", want, y)
		}
	}
	for _, bad := range []string{"snmp-only", "rm -rf", "PortChannel", "components/component/state/temperature"} {
		if strings.Contains(y, bad) {
			t.Errorf("%q must not appear in:\n%s", bad, y)
		}
	}
	if strings.Count(y, "component[name=Ethernet0]/transceiver/state") != 1 {
		t.Error("duplicate port names must collapse")
	}
	// Unchanged content is not rewritten (a rewrite restarts gnmic).
	st1, _ := os.Stat(path)
	os.Chtimes(path, st1.ModTime().Add(-time.Hour), st1.ModTime().Add(-time.Hour))
	WriteGnmicConfig(cfg, path, map[string][]string{"z9864f": {"Ethernet0", "Ethernet8"}})
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(st1.ModTime().Add(-time.Hour)) {
		t.Error("unchanged config was rewritten")
	}
	// Without known ports only the counters subscription is used; no wildcard component path.
	WriteGnmicConfig(cfg, path, nil)
	b, _ = os.ReadFile(path)
	if y := string(b); !strings.Contains(y, "subscriptions: [counters]\n") || strings.Contains(y, "components/component") {
		t.Errorf("port-less config:\n%s", y)
	}
	cfg.Switches = nil
	if n, _ := WriteGnmicConfig(cfg, path, nil); n != 0 {
		t.Error("expected 0 targets")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("config file should be removed when no gNMI switches remain")
	}
}
