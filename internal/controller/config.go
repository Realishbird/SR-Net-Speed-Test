// Package controller is the manager and collector that runs in the Proxmox LXC.
// It enrolls agents, schedules tests, stores results in Postgres and writes
// telemetry to VictoriaMetrics. It never carries test traffic itself.
package controller

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const DefaultConfigPath = "/etc/srnet/controller.json"

type Config struct {
	Listen             string `json:"listen"`     // :8443
	PublicURL          string `json:"public_url"` // https://10.0.0.5:8443, used in agent install commands
	DataDir            string `json:"data_dir"`
	DatabaseURL        string `json:"database_url"`
	VictoriaMetricsURL string `json:"victoriametrics_url"`
	TLSCert            string `json:"tls_cert"`
	TLSKey             string `json:"tls_key"`
	CACert             string `json:"ca_cert"`
	AdminUser          string `json:"admin_user"`
	AdminPasswordHash  string `json:"admin_password_hash"`
	InstallScriptURL   string `json:"install_script_url"`

	// Switches are collected in Phase 2 (gNMI first, SNMP fallback). They are
	// stored now so the deploy script can record them once.
	Switches []SwitchConfig `json:"switches,omitempty"`
}

type SwitchConfig struct {
	Name          string `json:"name"`
	Address       string `json:"address"`
	SNMPVersion   string `json:"snmp_version,omitempty"`   // 1 or 2c
	SNMPCommunity string `json:"snmp_community,omitempty"` // read-only
	GNMIPort      int    `json:"gnmi_port,omitempty"`
	GNMIUser      string `json:"gnmi_user,omitempty"`
	GNMIPassword  string `json:"gnmi_password,omitempty"`
	GNMITLS       bool   `json:"gnmi_tls,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Listen:             ":8443",
		DataDir:            "/var/lib/srnet",
		DatabaseURL:        "postgres:///srnet?host=/var/run/postgresql",
		VictoriaMetricsURL: "http://127.0.0.1:8428",
		TLSCert:            "/etc/srnet/tls/server.pem",
		TLSKey:             "/etc/srnet/tls/server-key.pem",
		CACert:             "/etc/srnet/tls/ca.pem",
		AdminUser:          "admin",
		InstallScriptURL:   "https://raw.githubusercontent.com/Realishbird/SR-Net-Speed-Test/main/install.sh",
	}
}

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func (c Config) Save(path string) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
