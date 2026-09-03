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
	case "pair":
		err = runPair(runContext, apiClient, commandArguments)
	case "nodes":
		err = runNodes(runContext, apiClient, commandArguments)
	case "claim":
		err = runClaim(runContext, apiClient, commandArguments)
	case "revoke":
		err = runRevoke(runContext, apiClient, commandArguments)
	case "exec":
		var exitCode int
		exitCode, err = runExec(runContext, apiClient, commandArguments)
		if err == nil {
			return exitCode
		}
	case "upload":
		err = runFileTransfer(runContext, apiClient, commandArguments, true)
	case "download":
		err = runFileTransfer(runContext, apiClient, commandArguments, false)
	case "job":
		err = runJob(runContext, apiClient, commandArguments)
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

func runPair(ctx context.Context, apiClient *client.Client, arguments []string) error {
	if len(arguments) != 0 {
		return errors.New("usage: ari pair")
	}
	opening, err := apiClient.OpenPairing(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("pairing code: %s\n", opening.Code)
	fmt.Printf("expires: %s (%d attempts)\n", opening.ExpiresAt.Local().Format(time.RFC3339), opening.RemainingAttempts)
	return nil
}

func runJob(ctx context.Context, apiClient *client.Client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: ari job {start|list|status|read|cancel|remove} ...")
	}
	switch arguments[0] {
	case "start":
		return runJobStart(ctx, apiClient, arguments[1:])
	case "list":
		if len(arguments) != 2 {
			return errors.New("usage: ari job list TARGET")
		}
		jobs, err := apiClient.ListJobs(ctx, arguments[1])
		if err != nil {
			return err
		}
		writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "JOB ID\tSTATE\tEXIT\tCREATED\tCOMMAND")
		for _, job := range jobs {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%s\n", job.ID, job.State, job.ExitCode, job.CreatedAt.Local().Format(time.RFC3339), jobDisplay(job))
		}
		return writer.Flush()
	case "status":
		if len(arguments) != 3 {
			return errors.New("usage: ari job status TARGET JOB_ID")
		}
		job, err := apiClient.JobStatus(ctx, arguments[1], arguments[2])
		if err != nil {
			return err
		}
		printJob(job)
		return nil
	case "read":
		return runJobRead(ctx, apiClient, arguments[1:])
	case "cancel":
		if len(arguments) != 3 {
			return errors.New("usage: ari job cancel TARGET JOB_ID")
		}
		job, err := apiClient.CancelJob(ctx, arguments[1], arguments[2])
		if err != nil {
			return err
		}
		printJob(job)
		return nil
	case "remove":
		if len(arguments) != 3 {
			return errors.New("usage: ari job remove TARGET JOB_ID")
		}
		return apiClient.RemoveJob(ctx, arguments[1], arguments[2])
	default:
		return fmt.Errorf("unknown job command %q", arguments[0])
	}
}

func runJobStart(ctx context.Context, apiClient *client.Client, arguments []string) error {
	flags := flag.NewFlagSet("ari job start", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "working directory on the node")
	timeout := flags.Duration("timeout", 0, "optional job runtime limit; zero means no limit")
	var command string
	flags.StringVar(&command, "command", "", "command line interpreted by the remote platform shell")
	flags.StringVar(&command, "c", "", "shorthand for --command")
	shell := flags.String("shell", "", "shell for --command: auto, posix, powershell, or cmd")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	rest := flags.Args()
	request := wire.ExecRequest{Command: command, Shell: *shell, Cwd: *cwd, TimeoutMillis: timeout.Milliseconds()}
	if command != "" {
		if len(rest) != 1 {
			return errors.New("usage: ari job start [OPTIONS] --command STRING TARGET")
		}
	} else {
		if len(rest) >= 2 && rest[1] == "--" {
			rest = append(rest[:1], rest[2:]...)
		}
		if len(rest) < 2 {
			return errors.New("usage: ari job start [OPTIONS] TARGET -- EXECUTABLE [ARG...]")
		}
		request.Argv = append([]string(nil), rest[1:]...)
	}
	if err := execspec.Validate(request); err != nil {
		return err
	}
	if *timeout < 0 {
		return errors.New("timeout cannot be negative")
	}
	job, err := apiClient.StartJob(ctx, rest[0], request)
	if err != nil {
		return err
	}
	printJob(job)
	return nil
}

func runJobRead(ctx context.Context, apiClient *client.Client, arguments []string) error {
	flags := flag.NewFlagSet("ari job read", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stdoutOffset := flags.Int64("stdout-offset", 0, "stdout byte offset")
	stderrOffset := flags.Int64("stderr-offset", 0, "stderr byte offset")
	limit := flags.Int("limit", 0, "maximum bytes from each stream")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) != 2 {
		return errors.New("usage: ari job read [OPTIONS] TARGET JOB_ID")
	}
	job, output, err := apiClient.ReadJob(ctx, rest[0], rest[1], *stdoutOffset, *stderrOffset, *limit)
	if err != nil {
		return err
	}
	_, _ = os.Stdout.Write(output.Stdout)
	_, _ = os.Stderr.Write(output.Stderr)
	fmt.Fprintf(os.Stderr, "ari: job %s state=%s next_stdout=%d next_stderr=%d stdout_eof=%t stderr_eof=%t\n", job.ID, job.State, output.NextStdoutOffset, output.NextStderrOffset, output.StdoutEOF, output.StderrEOF)
	return nil
}

func printJob(job wire.JobInfo) {
	fmt.Printf("%s state=%s exit=%d stdout=%d stderr=%d command=%s\n", job.ID, job.State, job.ExitCode, job.StdoutSize, job.StderrSize, jobDisplay(job))
}

func jobDisplay(job wire.JobInfo) string {
	if job.Command != "" {
		return job.Command
	}
	return fmt.Sprint(job.Argv)
}

func runFileTransfer(ctx context.Context, apiClient *client.Client, arguments []string, upload bool) error {
	name := "ari download"
	usageText := "usage: ari download [--overwrite] [--timeout DURATION] TARGET REMOTE_PATH LOCAL_PATH"
	if upload {
		name = "ari upload"
		usageText = "usage: ari upload [--overwrite] [--timeout DURATION] TARGET LOCAL_PATH REMOTE_PATH"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	overwrite := flags.Bool("overwrite", false, "replace an existing destination atomically")
	timeout := flags.Duration("timeout", 10*time.Minute, "transfer timeout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	paths := flags.Args()
	if len(paths) != 3 {
		return errors.New(usageText)
	}
	if *timeout < time.Millisecond {
		return errors.New("timeout must be at least 1ms")
	}
	transferContext, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var result wire.FileTransferResult
	var err error
	if upload {
		result, err = apiClient.UploadFile(transferContext, paths[0], paths[1], paths[2], *overwrite)
	} else {
		result, err = apiClient.DownloadFile(transferContext, paths[0], paths[1], paths[2], *overwrite)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%d bytes sha256:%s\n", result.Size, result.SHA256)
	return nil
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

func runRevoke(ctx context.Context, apiClient *client.Client, arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: ari revoke NODE_ID")
	}
	if err := apiClient.Revoke(ctx, arguments[0]); err != nil {
		return err
	}
	fmt.Printf("revoked %s\n", arguments[0])
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
  ari [global flags] pair
  ari [global flags] nodes
  ari [global flags] claim NODE_ID ALIAS
  ari [global flags] revoke NODE_ID
  ari [global flags] exec [OPTIONS] --command STRING TARGET
  ari [global flags] exec [OPTIONS] TARGET -- EXECUTABLE [ARG...]
  ari [global flags] upload [OPTIONS] TARGET LOCAL_PATH REMOTE_PATH
  ari [global flags] download [OPTIONS] TARGET REMOTE_PATH LOCAL_PATH
  ari [global flags] job start [OPTIONS] --command STRING TARGET
  ari [global flags] job {list|status|read|cancel|remove} ...
  ari [global flags] shell TARGET
  ari [global flags] proxy TARGET

Global flags must appear before the command. Use --relay-ssh breakglass@HOST
for the private management plane, or "ari proxy %h" as an OpenSSH
ProxyCommand.`)
}
