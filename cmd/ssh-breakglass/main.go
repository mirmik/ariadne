package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultUser = "breakglass"
	defaultTTL  = 15 * time.Minute
	minTTL      = time.Minute
	maxTTL      = 24 * time.Hour
	stateRoot   = "/run/ssh-breakglass"
)

type state struct {
	User       string    `json:"user"`
	Generation string    `json:"generation"`
	EnabledAt  time.Time `json:"enabled_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	TimerUnit  string    `json:"timer_unit"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		usage(stderr)
		return 2
	}
	if arguments[0] == "help" || arguments[0] == "-h" || arguments[0] == "--help" {
		usage(stdout)
		return 0
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "ssh-breakglass: must run as root (normally through sudo)")
		return 1
	}

	var err error
	switch arguments[0] {
	case "enable":
		err = runEnable(arguments[1:], stdout, stderr)
	case "disable":
		err = runDisable(arguments[1:], stdout, stderr)
	case "status":
		err = runStatus(arguments[1:], stdout, stderr)
	case "check":
		err = runCheck(arguments[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ssh-breakglass: unknown command %q\n", arguments[0])
		usage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "ssh-breakglass:", err)
		return 1
	}
	return 0
}

func runEnable(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ssh-breakglass enable", flag.ContinueOnError)
	flags.SetOutput(stderr)
	ttl := flags.Duration("ttl", defaultTTL, "password login window")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: ssh-breakglass enable [--ttl DURATION]")
	}
	if *ttl < minTTL || *ttl > maxTTL {
		return fmt.Errorf("ttl must be between %s and %s", minTTL, maxTTL)
	}
	if err := requireUser(defaultUser); err != nil {
		return err
	}

	return withLock(func() error {
		// Closing the previous window first makes enable fail closed.
		if err := lockPassword(defaultUser); err != nil {
			return err
		}
		password, err := randomText(24, "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789")
		if err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
		generation, err := randomText(24, "abcdefghijklmnopqrstuvwxyz234567")
		if err != nil {
			return fmt.Errorf("generate timer identity: %w", err)
		}
		now := time.Now().UTC()
		current := state{
			User:       defaultUser,
			Generation: generation,
			EnabledAt:  now,
			ExpiresAt:  now.Add(*ttl),
			TimerUnit:  "ssh-breakglass-" + defaultUser + "-" + generation[:12],
		}
		if err := writeState(current); err != nil {
			return err
		}
		if err := scheduleDisable(current, *ttl); err != nil {
			_ = os.Remove(statePath(defaultUser))
			return err
		}
		if err := setPassword(defaultUser, password); err != nil {
			_ = os.Remove(statePath(defaultUser))
			return err
		}

		fmt.Fprintf(stdout, "user: %s\npassword: %s\nexpires: %s\n", defaultUser, password, current.ExpiresAt.Format(time.RFC3339))
		return nil
	})
}

func runDisable(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ssh-breakglass disable", flag.ContinueOnError)
	flags.SetOutput(stderr)
	generation := flags.String("generation", "", "internal stale-timer guard")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: ssh-breakglass disable")
	}
	if err := requireUser(defaultUser); err != nil {
		return err
	}

	return withLock(func() error {
		if *generation != "" {
			current, err := readState(defaultUser)
			if errors.Is(err, os.ErrNotExist) || (err == nil && current.Generation != *generation) {
				return nil
			}
			if err != nil {
				return err
			}
		}
		if err := lockPassword(defaultUser); err != nil {
			return err
		}
		if err := os.Remove(statePath(defaultUser)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state: %w", err)
		}
		fmt.Fprintf(stdout, "password login disabled for %s\n", defaultUser)
		return nil
	})
}

func runStatus(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ssh-breakglass status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: ssh-breakglass status")
	}
	if err := requireUser(defaultUser); err != nil {
		return err
	}
	passwordStatus, err := commandOutput("passwd", "-S", defaultUser)
	if err != nil {
		return fmt.Errorf("read password status: %w", err)
	}
	current, stateErr := readState(defaultUser)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return stateErr
	}
	fmt.Fprintf(stdout, "account: %s\n", strings.TrimSpace(passwordStatus))
	if stateErr == nil {
		fmt.Fprintf(stdout, "window: enabled\nexpires: %s\ntimer: %s.timer\n", current.ExpiresAt.Format(time.RFC3339), current.TimerUnit)
	} else {
		fmt.Fprintln(stdout, "window: disabled")
	}
	return nil
}

func runCheck(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ssh-breakglass check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: ssh-breakglass check")
	}
	if err := requireUser(defaultUser); err != nil {
		return err
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return errors.New("systemd-run is required")
	}
	configuration, err := commandOutput("sshd", "-T", "-C", "user="+defaultUser+",host=localhost,addr=127.0.0.1")
	if err != nil {
		return fmt.Errorf("inspect effective sshd configuration: %w", err)
	}
	passwordAllowed := false
	for _, line := range strings.Split(configuration, "\n") {
		if strings.TrimSpace(line) == "passwordauthentication yes" {
			passwordAllowed = true
			break
		}
	}
	if !passwordAllowed {
		return fmt.Errorf("sshd does not allow password authentication for user %s", defaultUser)
	}
	fmt.Fprintf(stdout, "configuration is ready for %s\n", defaultUser)
	return nil
}

func scheduleDisable(current state, ttl time.Duration) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	seconds := int64(ttl.Round(time.Second) / time.Second)
	command := exec.Command("systemd-run",
		"--quiet",
		"--collect",
		"--unit="+current.TimerUnit,
		fmt.Sprintf("--on-active=%ds", seconds),
		"--timer-property=AccuracySec=1s",
		executable, "disable", "--generation", current.Generation,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("schedule automatic disable: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func requireUser(user string) error {
	if err := exec.Command("id", "-u", user).Run(); err != nil {
		return fmt.Errorf("user %q does not exist", user)
	}
	return nil
}

func lockPassword(user string) error {
	if output, err := exec.Command("passwd", "-l", user).CombinedOutput(); err != nil {
		return fmt.Errorf("lock password for %s: %w: %s", user, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func setPassword(user, password string) error {
	command := exec.Command("chpasswd")
	command.Stdin = strings.NewReader(user + ":" + password + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("set temporary password for %s: %w: %s", user, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func commandOutput(name string, arguments ...string) (string, error) {
	output, err := exec.Command(name, arguments...).CombinedOutput()
	return string(output), err
}

func randomText(length int, alphabet string) (string, error) {
	if length <= 0 || len(alphabet) == 0 || len(alphabet) > 256 {
		return "", errors.New("invalid random text parameters")
	}
	// Discard the uneven tail instead of introducing modulo bias.
	limit := 256 - 256%len(alphabet)
	result := make([]byte, 0, length)
	buffer := make([]byte, length)
	for len(result) < length {
		if _, err := rand.Read(buffer); err != nil {
			return "", err
		}
		for _, value := range buffer {
			if int(value) >= limit {
				continue
			}
			result = append(result, alphabet[int(value)%len(alphabet)])
			if len(result) == length {
				break
			}
		}
	}
	return string(result), nil
}

func withLock(action func() error) error {
	if err := os.MkdirAll(stateRoot, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(stateRoot, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck
	return action()
}

func statePath(user string) string {
	return filepath.Join(stateRoot, user+".json")
}

func writeState(current state) error {
	encoded, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	temporary, err := os.CreateTemp(stateRoot, ".state-*")
	if err != nil {
		return fmt.Errorf("create state: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect state: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(temporaryName, statePath(current.User)); err != nil {
		return fmt.Errorf("publish state: %w", err)
	}
	return nil
}

func readState(user string) (state, error) {
	encoded, err := os.ReadFile(statePath(user))
	if err != nil {
		return state{}, err
	}
	var current state
	if err := json.Unmarshal(encoded, &current); err != nil {
		return state{}, fmt.Errorf("decode state: %w", err)
	}
	if current.User != user || current.Generation == "" {
		return state{}, errors.New("invalid state file")
	}
	return current, nil
}

func usage(output io.Writer) {
	fmt.Fprintln(output, `Usage:
  ssh-breakglass enable [--ttl 15m]
  ssh-breakglass disable
  ssh-breakglass status
  ssh-breakglass check`)
}
