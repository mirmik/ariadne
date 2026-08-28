package connector

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mirmik/ariadne/internal/wire"
)

func TestLocalExecutorCapturesExitAndOutput(t *testing.T) {
	executor := helperExecutor(1024)
	result := executor.Execute(context.Background(), helperRequest("exit", "7"))
	if result.ExitCode != 7 {
		t.Fatalf("exit code is %d, expected 7", result.ExitCode)
	}
	if string(result.Stdout) != "stdout\n" || string(result.Stderr) != "stderr\n" {
		t.Fatalf("unexpected output: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
	if result.Error != "" {
		t.Fatalf("non-zero exit was treated as transport error: %s", result.Error)
	}
}

func TestLocalExecutorLimitsOutput(t *testing.T) {
	executor := helperExecutor(10)
	result := executor.Execute(context.Background(), helperRequest("spam"))
	if len(result.Stdout) != 10 || !result.StdoutTruncated {
		t.Fatalf("stdout was not capped: len=%d truncated=%v", len(result.Stdout), result.StdoutTruncated)
	}
	if len(result.Stderr) != 10 || !result.StderrTruncated {
		t.Fatalf("stderr was not capped: len=%d truncated=%v", len(result.Stderr), result.StderrTruncated)
	}
}

func TestLocalExecutorHonorsCancellation(t *testing.T) {
	executor := helperExecutor(1024)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := executor.Execute(ctx, helperRequest("sleep"))
	if !result.TimedOut || result.Error != "command timed out" {
		t.Fatalf("timeout was not reported: %#v", result)
	}
	if result.DurationMillis <= 0 || result.DurationMillis > 2000 {
		t.Fatalf("unexpected duration: %dms", result.DurationMillis)
	}
}

func TestSafeEnvironmentDropsCredentials(t *testing.T) {
	filtered := safeEnvironment([]string{
		"PATH=/bin",
		"LC_TEST=value",
		"ARIADNE_PRIVATE=secret",
		"DATABASE_PASSWORD=secret",
	})
	joined := strings.Join(filtered, "\n")
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "LC_TEST=value") {
		t.Fatalf("safe values were removed: %v", filtered)
	}
	if strings.Contains(joined, "secret") {
		t.Fatalf("credential-like values leaked: %v", filtered)
	}
}

func TestSafeEnvironmentPreservesWindowsNamesCaseInsensitively(t *testing.T) {
	filtered := safeEnvironmentForOS([]string{
		"Path=C:\\Windows\\System32",
		"SystemRoot=C:\\Windows",
		"UserProfile=C:\\Users\\radio",
		"ProgramFiles(x86)=C:\\Program Files (x86)",
		"Database_Password=secret",
	}, "windows")
	joined := strings.Join(filtered, "\n")
	for _, expected := range []string{"Path=", "SystemRoot=", "UserProfile=", "ProgramFiles(x86)="} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("Windows environment variable %q was removed: %v", expected, filtered)
		}
	}
	if strings.Contains(joined, "secret") {
		t.Fatalf("credential-like value leaked: %v", filtered)
	}
}

func helperExecutor(maxOutput int) LocalExecutor {
	environment := append([]string(nil), os.Environ()...)
	environment = append(environment, "ARIADNE_EXECUTOR_HELPER=1")
	return LocalExecutor{MaxOutputBytes: maxOutput, Environment: environment}
}

func helperRequest(arguments ...string) wire.ExecRequest {
	argv := []string{os.Args[0], "-test.run=TestExecutorHelperProcess", "--"}
	argv = append(argv, arguments...)
	return wire.ExecRequest{Argv: argv}
}

func TestExecutorHelperProcess(t *testing.T) {
	if os.Getenv("ARIADNE_EXECUTOR_HELPER") != "1" {
		return
	}
	separator := 0
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index + 1
			break
		}
	}
	if separator == 0 || separator >= len(os.Args) {
		os.Exit(90)
	}
	switch os.Args[separator] {
	case "exit":
		fmt.Fprintln(os.Stdout, "stdout")
		fmt.Fprintln(os.Stderr, "stderr")
		var exitCode int
		if _, err := fmt.Sscan(os.Args[separator+1], &exitCode); err != nil {
			os.Exit(91)
		}
		os.Exit(exitCode)
	case "spam":
		fmt.Fprint(os.Stdout, strings.Repeat("o", 100))
		fmt.Fprint(os.Stderr, strings.Repeat("e", 100))
		os.Exit(0)
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	default:
		os.Exit(92)
	}
}
