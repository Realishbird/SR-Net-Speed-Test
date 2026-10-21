// Command srnet-agent runs benchmarks and collects telemetry on a test host.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/agent"
	"github.com/Realishbird/SR-Net-Speed-Test/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		cfgPath := fs.String("config", agent.DefaultConfigPath, "config file")
		fs.Parse(args)
		cfg, err := agent.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		err = agent.Run(ctx, cfg)
		if agent.IsRestart(err) {
			os.Exit(0) // systemd Restart=always brings the launcher back with the new binary
		}
		if err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
	case "enroll":
		fs := flag.NewFlagSet("enroll", flag.ExitOnError)
		cfgPath := fs.String("config", agent.DefaultConfigPath, "config file to write")
		controller := fs.String("controller", "", "controller URL, e.g. https://10.0.0.5:8443")
		token := fs.String("token", "", "one-time enrollment token")
		ca := fs.String("ca", "/etc/srnet/ca.pem", "controller CA certificate")
		fs.Parse(args)
		if *controller == "" || *token == "" {
			log.Fatal("--controller and --token are required")
		}
		if err := agent.Enroll(ctx, agent.Config{Controller: *controller, CAFile: *ca}, *token, *cfgPath); err != nil {
			log.Fatalf("enroll: %v", err)
		}
		fmt.Println("enrolled; config written to", *cfgPath)
	case "inventory":
		inv := agent.Inventory(ctx)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(inv)
	case "sample":
		s := agent.NewSampler()
		s.SetPorts(agent.DiscoverPorts(ctx))
		s.Collect(ctx) // first pass primes the rate counters
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(s.Collect(ctx))
	case "version":
		fmt.Println(version.Version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: srnet-agent <command>

  run        run the agent (systemd starts this)
  enroll     register with the controller using a one-time token
  inventory  print the RDMA ports and host facts this agent reports
  sample     print one telemetry sample
  version    print the agent version`)
	os.Exit(2)
}
