package controller

import (
	"testing"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestMergeSwitch(t *testing.T) {
	old := SwitchConfig{Name: "z9864f", Address: "10.0.0.2", SNMPCommunity: "oldro", GNMIUser: "admin", GNMIPassword: "oldpw", GNMIPort: 8080, GNMITLS: true}

	// Blank secrets on an edit keep the stored ones; the port defaults to 8080.
	sw, err := mergeSwitch(api.SwitchRequest{Name: "z9864f", Address: "10.0.0.9", SNMPEnabled: true, GNMIEnabled: true, GNMIUser: "admin", GNMITLS: true}, old, true)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Address != "10.0.0.9" || sw.SNMPCommunity != "oldro" || sw.GNMIPassword != "oldpw" || sw.GNMIPort != 8080 || sw.SNMPVersion != "2c" {
		t.Errorf("merge kept wrong values: %+v", sw)
	}

	// Disabling gNMI drops its settings entirely.
	sw, err = mergeSwitch(api.SwitchRequest{Name: "z9864f", Address: "10.0.0.2", SNMPEnabled: true, SNMPVersion: "1"}, old, true)
	if err != nil || sw.GNMIUser != "" || sw.GNMIPassword != "" || sw.SNMPVersion != "1" {
		t.Errorf("gNMI should be dropped: %+v err=%v", sw, err)
	}

	// A new switch needs its secrets.
	for _, req := range []api.SwitchRequest{
		{Name: "new", Address: "10.0.0.3", SNMPEnabled: true},
		{Name: "new", Address: "10.0.0.3", GNMIEnabled: true, GNMIUser: "admin"},
		{Name: "new", Address: "10.0.0.3"},
		{Name: "bad name", Address: "10.0.0.3", SNMPEnabled: true, SNMPCommunity: "x"},
		{Name: "new", Address: "", SNMPEnabled: true, SNMPCommunity: "x"},
	} {
		if _, err := mergeSwitch(req, SwitchConfig{}, false); err == nil {
			t.Errorf("expected an error for %+v", req)
		}
	}
}

func TestSNMPCollectorSet(t *testing.T) {
	c := newSNMPCollector([]SwitchConfig{{Name: "a", Address: "10.0.0.1", SNMPCommunity: "x"}, {Name: "g", Address: "10.0.0.2", GNMIUser: "admin"}}, newMetrics(""))
	if got := c.current(); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("expected only the SNMP switch, got %+v", got)
	}
	c.mu.Lock()
	c.status["a"] = snmpStatus{Ports: 3}
	c.mu.Unlock()
	c.set([]SwitchConfig{{Name: "b", Address: "10.0.0.3", SNMPCommunity: "y"}})
	if got := c.current(); len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("set did not replace the list: %+v", got)
	}
	if _, ok := c.statuses()["a"]; ok {
		t.Error("status of a removed switch should be dropped")
	}
	select {
	case <-c.kick:
	default:
		t.Error("set should wake the poll loop")
	}
}
