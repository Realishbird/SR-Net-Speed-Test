// Command srnet-controller is the manager and collector that runs in the Proxmox LXC.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
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
		fp, _ := controller.CAFingerprint(cfg.CACert)
		fmt.Printf("config written to %s\nCA fingerprint (sha256): %s\n", *cfgPath, fp)
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
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: srnet-controller <command> [--config /etc/srnet/controller.json]

  init     write the config, TLS certificates and admin password
  serve    run the controller (systemd starts this)
  passwd   change the admin password
  version  print the version`)
	os.Exit(2)
}
