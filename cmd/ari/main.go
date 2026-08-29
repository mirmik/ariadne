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
	"github.com/mirmik/ariadne/internal/cliargs"
	"github.com/mirmik/ariadne/internal/client"
	"github.com/mirmik/ariadne/internal/execspec"
	"github.com/mirmik/ariadne/internal/managementauth"
	"github.com/mirmik/ariadne/internal/sshtunnel"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/mirmik/ariadne/internal/wire"
)

var version = "dev"

func main() {
	os.Exit(run(cliargs.Current()))
}

func run(arguments []string) int {
	defaultManagementTokenPath, err := managementauth.DefaultPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ari:", err)
		return 2
	}
	flags := flag.NewFlagSet("ari", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	relayURL := flags.String("relay", "http://127.0.0.1:8088", "management-plane relay base URL")
	relaySSH := flags.String("relay-ssh", "", "reach management plane through OpenSSH (user@host or user@host:port)")
	managementTokenPath := flags.String("management-token-file", defaultManagementTokenPath, "management bearer token file")
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
	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *relaySSH != "" {
		tunnel, tunnelErr := sshtunnel.Start(runContext, sshtunnel.Config{
			Destination:   *relaySSH,
			RemoteAddress: "127.0.0.1:8088",
		})
		if tunnelErr != nil {
			fmt.Fprintln(os.Stderr, "ari:", tunnelErr)
			return 1
		}
		defer tunnel.Close()
		*relayURL = tunnel.URL
	}
	if err := transport.ValidateRelayURL(*relayURL, *allowInsecureRelay); err != nil {
		fmt.Fprintln(os.Stderr, "ari:", err)
		return 2
	}
	managementToken, err := managementauth.Load(*managementTokenPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ari:", err)
		return 2
	}
	apiClient, err := client.New(client.Config{RelayURL: *relayURL, ManagementToken: managementToken})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ari:", err)
		return 2
	}

	command := flags.Arg(0)
	commandArguments := flags.Args()[1:]
	switch command {
	case "nodes":
		err = runNodes(runContext, apiClient, commandArguments)
	case "claim":
		err = runClaim(runContext, apiClient, commandArguments)
	case "exec":
		var exitCode int
		exitCode, err = runExec(runContext, apiClient, commandArguments)
		if err == nil {
			return exitCode
		}
	case "shell":
		var exitCode int
		exitCode, err = runShell(runContext, apiClient, commandArguments)
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
		alias := node.Alias
		if !node.AliasClaimed {
			alias += "?"
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", alias, node.ID, platform, node.ConnectedAt.Local().Format(time.RFC3339))
	}
	return writer.Flush()
}

func runClaim(ctx context.Context, apiClient *client.Client, arguments []string) error {
	if len(arguments) != 2 {
		return errors.New("usage: ari claim NODE_ID ALIAS")
	}
	node, err := apiClient.Claim(ctx, arguments[0], arguments[1])
	if err != nil {
		return err
	}
	fmt.Printf("claimed %s as %s\n", node.ID, node.Alias)
	return nil
}

func runExec(ctx context.Context, apiClient *client.Client, arguments []string) (int, error) {
	flags := flag.NewFlagSet("ari exec", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "working directory on the node")
	timeout := flags.Duration("timeout", 30*time.Second, "command timeout")
	var command string
	flags.StringVar(&command, "command", "", "command line interpreted by the remote platform shell")
	flags.StringVar(&command, "c", "", "shorthand for --command")
	shell := flags.String("shell", "", "shell for --command: auto, posix, powershell, or cmd")
	if err := flags.Parse(arguments); err != nil {
		return 1, err
	}
	rest := flags.Args()
	request := wire.ExecRequest{Command: command, Shell: *shell, Cwd: *cwd}
	if command != "" {
		if len(rest) != 1 {
			return 1, errors.New("usage: ari exec [OPTIONS] --command STRING TARGET")
		}
	} else {
		if len(rest) >= 2 && rest[1] == "--" {
			rest = append(rest[:1], rest[2:]...)
		}
		if len(rest) < 2 {
			return 1, errors.New("usage: ari exec [OPTIONS] TARGET -- EXECUTABLE [ARG...]")
		}
		request.Argv = append([]string(nil), rest[1:]...)
	}
	if err := execspec.Validate(request); err != nil {
		return 1, err
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
	request.TimeoutMillis = timeout.Milliseconds()
	result, err := apiClient.Exec(requestContext, rest[0], request)
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
  ari [global flags] claim NODE_ID ALIAS
  ari [global flags] exec [OPTIONS] --command STRING TARGET
  ari [global flags] exec [OPTIONS] TARGET -- EXECUTABLE [ARG...]
  ari [global flags] shell TARGET
  ari [global flags] proxy TARGET

Global flags must appear before the command. Use --relay-ssh breakglass@HOST
for the private management plane, or "ari proxy %h" as an OpenSSH
ProxyCommand.`)
}
