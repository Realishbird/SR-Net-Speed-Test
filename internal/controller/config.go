// Package controller is the manager and collector that runs in the Proxmox LXC.
// It enrolls agents, schedules tests, stores results in Postgres and writes
// telemetry to VictoriaMetrics. It never carries test traffic itself.
package controller

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const DefaultConfigPath = "/etc/srnet/controller.json"

type Config struct {
	Listen             string  `json:"listen"`     // :8443
	PublicURL          string  `json:"public_url"` // https://10.0.0.5:8443, used in agent install commands
	DataDir            string  `json:"data_dir"`
	DatabaseURL        string  `json:"database_url"`
	VictoriaMetricsURL string  `json:"victoriametrics_url"`
	TLSCert            string  `json:"tls_cert"`
	TLSKey             string  `json:"tls_key"`
	CACert             string  `json:"ca_cert"`
	AdminUser          string  `json:"admin_user"`
	AdminPasswordHash  string  `json:"admin_password_hash"`
	InstallScriptURL   string  `json:"install_script_url"`
	GrafanaURL         string  `json:"grafana_url,omitempty"` // shown in the UI; defaults to port 3000 on the same host
	GnmicConfigPath    string  `json:"gnmic_config_path"`
	GnmicPath          string  `json:"gnmic_path"` // gnmic binary, used for gNMI Get requests (LLDP table)
	GrafanaDashDir     string  `json:"grafana_dashboard_dir"`
	AlertWebhookURL    string  `json:"alert_webhook_url,omitempty"` // POSTed a JSON body when an alert opens or resolves
	TransceiverMaxC    float64 `json:"transceiver_max_c,omitempty"` // alert when a module reports this temperature or more (default 70)

	// Switches to collect port counters from: gNMI through gnmic when GNMIUser
	// is set, SNMP v1/v2c from the controller itself when SNMPCommunity is set.
	Switches []SwitchConfig `json:"switches,omitempty"`

	path string // file this config was loaded from, for SaveBack
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
		GnmicConfigPath:    "/etc/srnet/gnmic.yaml",
		GnmicPath:          "/usr/local/bin/gnmic",              // where install-controller.sh puts it
		GrafanaDashDir:     "/var/lib/grafana/dashboards/srnet", // where install-controller.sh points Grafana's file provisioner
	}
}

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	c.path = path
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// SaveBack writes the config to the file it was loaded from.
// CLIPasswordFile is where the srnet CLI reads the admin password
// (admin-password beside the config file), or "" when the config was not
// loaded from a file.
func (c Config) CLIPasswordFile() string {
	if c.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.path), "admin-password")
}

func (c Config) SaveBack() error {
	if c.path == "" {
		return errors.New("config was not loaded from a file")
	}
	return c.Save(c.path)
}

func (c Config) Save(path string) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
