package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/mirmik/ariadne/internal/client"
	"github.com/mirmik/ariadne/internal/managementauth"
	"github.com/mirmik/ariadne/internal/mcpserver"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ariadne-mcp:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	defaultTokenPath, err := managementauth.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("ariadne-mcp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	relayURL := flags.String("relay", "http://127.0.0.1:8088", "management-plane relay base URL")
	managementTokenPath := flags.String("management-token-file", defaultTokenPath, "management bearer token file")
	allowInsecureRelay := flags.Bool("allow-insecure-relay", false, "allow plaintext relay outside loopback")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if err := transport.ValidateRelayURL(*relayURL, *allowInsecureRelay); err != nil {
		return err
	}
	managementToken, err := managementauth.Load(*managementTokenPath)
	if err != nil {
		return err
	}
	apiClient, err := client.New(client.Config{RelayURL: *relayURL, ManagementToken: managementToken})
	if err != nil {
		return err
	}

	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	server := mcpserver.New(apiClient, version)
	return server.Run(runContext, &mcp.StdioTransport{})
}
