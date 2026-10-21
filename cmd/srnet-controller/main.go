// Command srnet-controller is the manager and collector that runs in the Proxmox LXC.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/controller"
	"github.com/Realishbird/SR-Net-Speed-Test/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", controller.DefaultConfigPath, "config file")
	switch cmd {
	case "init":
		publicURL := fs.String("public-url", "", "URL agents use to reach this controller, e.g. https://10.0.0.5:8443")
		hosts := fs.String("cert-hosts", "", "comma-separated hostnames and IPs for the TLS certificate")
		password := fs.String("admin-password-file", "", "file holding the admin password (read once, not stored)")
		fs.Parse(args)
		if *publicURL == "" || *password == "" {
			log.Fatal("--public-url and --admin-password-file are required")
		}
		cfg := controller.DefaultConfig()
		if old, err := controller.LoadConfig(*cfgPath); err == nil {
			cfg = old // re-running init keeps database and switch settings
		}
		cfg.PublicURL = strings.TrimRight(*publicURL, "/")
		if err := setPassword(&cfg, *password); err != nil {
			log.Fatal(err)
		}
		if _, err := os.Stat(cfg.CACert); errors.Is(err, os.ErrNotExist) {
			if err := controller.GenerateCerts(cfg, strings.Split(*hosts, ",")); err != nil {
				log.Fatalf("certificates: %v", err)
			}
		}
		if err := cfg.Save(*cfgPath); err != nil {
			log.Fatal(err)
		}
		if err := writeDashboards(cfg); err != nil {
			log.Printf("grafana dashboards: %v", err)
		}
		fp, _ := controller.CAFingerprint(cfg.CACert)
		fmt.Printf("config written to %s\nCA fingerprint (sha256): %s\n", *cfgPath, fp)
	case "dashboards":
		// srnet-controller dashboards --dir /var/lib/grafana/dashboards/srnet
		// Writes the bundled Grafana dashboards where Grafana's file provisioner
		// reads them. The installer runs this as root so the files end up in a
		// directory the grafana user can read.
		dir := fs.String("dir", "", "directory for the dashboard JSON files (default: grafana_dashboard_dir from the config)")
		fs.Parse(args)
		cfg := controller.DefaultConfig()
		if old, err := controller.LoadConfig(*cfgPath); err == nil {
			cfg = old
		}
		if *dir != "" {
			cfg.GrafanaDashDir = *dir
		}
		if err := writeDashboards(cfg); err != nil {
			log.Fatalf("grafana dashboards: %v", err)
		}
		fmt.Printf("dashboards written to %s\n", cfg.GrafanaDashDir)
	case "switch":
		// srnet-controller switch add --name sw1 --address 10.0.0.2 --snmp-community public --gnmi-user admin --gnmi-password x --gnmi-tls
		name := fs.String("name", "", "switch name")
		addr := fs.String("address", "", "management IP or host")
		remove := fs.Bool("remove", false, "remove the named switch")
		snmpVer := fs.String("snmp-version", "2c", "1 or 2c")
		community := fs.String("snmp-community", "", "read-only community")
		gnmiUser := fs.String("gnmi-user", "", "gNMI username")
		gnmiPass := fs.String("gnmi-password", "", "gNMI password")
		gnmiPort := fs.Int("gnmi-port", 8080, "gNMI port")
		gnmiTLS := fs.Bool("gnmi-tls", true, "gNMI over TLS (certificate not verified)")
		fs.Parse(args)
		if *name == "" {
			log.Fatal("--name is required")
		}
		cfg, err := controller.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		var kept []controller.SwitchConfig
		for _, sw := range cfg.Switches {
			if sw.Name != *name {
				kept = append(kept, sw)
			}
		}
		if !*remove {
			if *addr == "" {
				log.Fatal("--address is required")
			}
			kept = append(kept, controller.SwitchConfig{Name: *name, Address: *addr, SNMPVersion: *snmpVer, SNMPCommunity: *community,
				GNMIPort: *gnmiPort, GNMIUser: *gnmiUser, GNMIPassword: *gnmiPass, GNMITLS: *gnmiTLS})
		}
		cfg.Switches = kept
		if err := cfg.Save(*cfgPath); err != nil {
			log.Fatal(err)
		}
		n, err := controller.WriteGnmicConfig(cfg, cfg.GnmicConfigPath, nil) // the running controller adds the transceiver ports
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%d switch(es) configured, %d with gNMI; restart srnet-controller and gnmic to apply\n", len(kept), n)
	case "passwd":
		password := fs.String("admin-password-file", "", "file holding the new admin password; prompts when empty")
		fs.Parse(args)
		cfg, err := controller.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		if err := setPassword(&cfg, *password); err != nil {
			log.Fatal(err)
		}
		if err := cfg.Save(*cfgPath); err != nil {
			log.Fatal(err)
		}
		// The srnet CLI reads the password from this file; a stale copy
		// would lock it out after the change.
		if pwFile := filepath.Join(filepath.Dir(*cfgPath), "admin-password"); *password != pwFile {
			if st, err := os.Stat(pwFile); err == nil && st.Mode().IsRegular() {
				if err := os.WriteFile(pwFile, []byte(lastPassword+"\n"), 0o600); err != nil {
					log.Printf("could not update %s for the srnet CLI: %v", pwFile, err)
				}
			}
		}
		fmt.Println("admin password updated; restart srnet-controller to apply")
	case "serve":
		fs.Parse(args)
		cfg, err := controller.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		srv, err := controller.NewServer(ctx, cfg)
		if err != nil {
			log.Fatal(err)
		}
		defer srv.Close()
		hs := &http.Server{
			Addr:              cfg.Listen,
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
			// Browsers that have not accepted the self-signed certificate
			// abort the handshake on every retry; one line per attempt is
			// noise, not news. Everything else the server logs stays.
			ErrorLog: log.New(dropTLSHandshake{os.Stderr}, "", log.LstdFlags),
		}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hs.Shutdown(sctx)
		}()
		log.Printf("srnet-controller %s listening on %s", version.Version, cfg.Listen)
		if err := hs.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case "version":
		fmt.Println(version.Version)
	default:
		usage()
	}
}

// writeDashboards drops the bundled Grafana dashboards where provisioning picks them up.
func writeDashboards(cfg controller.Config) error {
	dash, err := controller.GrafanaDashboards()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.GrafanaDashDir, 0o755); err != nil {
		return err
	}
	for name, b := range dash {
		if err := os.WriteFile(cfg.GrafanaDashDir+"/"+name, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// dropTLSHandshake swallows net/http's "TLS handshake error" lines.
type dropTLSHandshake struct{ w io.Writer }

func (d dropTLSHandshake) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("TLS handshake error")) {
		return len(p), nil
	}
	return d.w.Write(p)
}

// lastPassword is the password setPassword last hashed, for the passwd
// command to mirror into the CLI's password file.
var lastPassword string

func setPassword(cfg *controller.Config, file string) error {
	var pw string
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		pw = strings.TrimRight(string(b), "\r\n")
	} else {
		fmt.Fprint(os.Stderr, "new admin password: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(line, "\r\n")
	}
	if len(pw) < 8 {
		return errors.New("admin password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	cfg.AdminPasswordHash = string(h)
	lastPassword = pw
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: srnet-controller <command> [--config /etc/srnet/controller.json]

  init     write the config, TLS certificates, admin password and Grafana dashboards
  dashboards  write the bundled Grafana dashboards to --dir, for Grafana's file provisioner
  serve    run the controller (systemd starts this)
  passwd   change the admin password
  switch   add or remove a switch to collect from (--name, --address, --snmp-community, --gnmi-user ...)
  version  print the version`)
	os.Exit(2)
}
