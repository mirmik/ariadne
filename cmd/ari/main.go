package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/client"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/mirmik/ariadne/internal/wire"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	flags := flag.NewFlagSet("ari", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	relayURL := flags.String("relay", "http://127.0.0.1:8088", "relay base URL")
	token := flags.String("token", os.Getenv("ARIADNE_TOKEN"), "bearer token (prefer ARIADNE_TOKEN)")
	insecureNoAuth := flags.Bool("insecure-no-auth", false, "connect without authentication")
	allowInsecureRelay := flags.Bool("allow-insecure-relay", false, "allow plaintext relay outside loopback")
	showVersion := flags.Bool("version", false, "print version and exit")
	flags.Usage = usage
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	if flags.NArg() == 0 {
		usage()
		return 2
	}
	if *token == "" && !*insecureNoAuth {
		fmt.Fprintln(os.Stderr, "ari: ARIADNE_TOKEN is required unless --insecure-no-auth is explicitly set")
		return 2
	}
	if err := transport.ValidateRelayURL(*relayURL, *allowInsecureRelay); err != nil {
		fmt.Fprintln(os.Stderr, "ari:", err)
		return 2
	}
	apiClient, err := client.New(client.Config{RelayURL: *relayURL, Token: *token})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ari:", err)
		return 2
	}

	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	command := flags.Arg(0)
	commandArguments := flags.Args()[1:]
	switch command {
	case "nodes":
		err = runNodes(runContext, apiClient, commandArguments)
	case "exec":
		var exitCode int
		exitCode, err = runExec(runContext, apiClient, commandArguments)
		if err == nil {
			return exitCode
		}
	case "proxy":
		err = runProxy(runContext, apiClient, commandArguments)
	case "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "ari: unknown command %q\n", command)
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ari:", err)
		return 1
	}
	return 0
}

func runNodes(ctx context.Context, apiClient *client.Client, arguments []string) error {
	if len(arguments) != 0 {
		return errors.New("usage: ari nodes")
	}
	nodes, err := apiClient.Nodes(ctx)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ALIAS\tNODE ID\tPLATFORM\tCONNECTED")
	for _, node := range nodes {
		platform := node.Platform + "/" + node.Architecture
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", node.Alias, node.ID, platform, node.ConnectedAt.Local().Format(time.RFC3339))
	}
	return writer.Flush()
}

func runExec(ctx context.Context, apiClient *client.Client, arguments []string) (int, error) {
	flags := flag.NewFlagSet("ari exec", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "working directory on the node")
	timeout := flags.Duration("timeout", 30*time.Second, "command timeout")
	if err := flags.Parse(arguments); err != nil {
		return 1, err
	}
	rest := flags.Args()
	if len(rest) >= 2 && rest[1] == "--" {
		rest = append(rest[:1], rest[2:]...)
	}
	if len(rest) < 2 {
		return 1, errors.New("usage: ari exec [--cwd DIR] [--timeout DURATION] TARGET -- COMMAND [ARG...]")
	}
	if *timeout < time.Millisecond {
		return 1, errors.New("timeout must be at least 1ms")
	}
	const clientGrace = 10 * time.Second
	if *timeout > time.Duration(1<<63-1)-clientGrace {
		return 1, errors.New("timeout is too large")
	}
	requestContext, cancel := context.WithTimeout(ctx, *timeout+clientGrace)
	defer cancel()
	result, err := apiClient.Exec(requestContext, rest[0], wire.ExecRequest{
		Argv:          rest[1:],
		Cwd:           *cwd,
		TimeoutMillis: timeout.Milliseconds(),
	})
	if err != nil {
		return 1, err
	}
	_, _ = os.Stdout.Write(result.Stdout)
	_, _ = os.Stderr.Write(result.Stderr)
	if result.StdoutTruncated {
		fmt.Fprintln(os.Stderr, "ari: remote stdout was truncated")
	}
	if result.StderrTruncated {
		fmt.Fprintln(os.Stderr, "ari: remote stderr was truncated")
	}
	if result.Error != "" {
		return 1, errors.New(result.Error)
	}
	if result.ExitCode < 0 || result.ExitCode > 255 {
		return 1, nil
	}
	return result.ExitCode, nil
}

func runProxy(ctx context.Context, apiClient *client.Client, arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: ari proxy TARGET")
	}
	connection, err := apiClient.DialStream(ctx, arguments[0], "ssh")
	if err != nil {
		return err
	}
	networkConnection := websocket.NetConn(ctx, connection, websocket.MessageBinary)
	defer networkConnection.Close()
	errorChannel := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(networkConnection, os.Stdin)
		errorChannel <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(os.Stdout, networkConnection)
		errorChannel <- copyErr
	}()
	err = <-errorChannel
	_ = networkConnection.Close()
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	status := websocket.CloseStatus(err)
	if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway || status == websocket.StatusNoStatusRcvd {
		return nil
	}
	return err
}

func usage() {
	_, _ = fmt.Fprintln(os.Stderr, `Usage:
  ari [global flags] nodes
  ari [global flags] exec [--cwd DIR] [--timeout DURATION] TARGET -- COMMAND [ARG...]
  ari [global flags] proxy TARGET

Global flags must appear before the command. Use "ari proxy %h" as an
OpenSSH ProxyCommand.`)
}
