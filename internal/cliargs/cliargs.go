package cliargs

import (
	"os"
	"path/filepath"
	"runtime"
)

const termuxSelfExeEnvironment = "TERMUX_EXEC__PROC_SELF_EXE"

// FromArgv returns command arguments without argv[0]. When Termux starts an
// Android binary through its linker wrapper, the executable path identified by
// TERMUX_EXEC__PROC_SELF_EXE can also appear as argv[1]. Remove only that
// wrapper-injected argument; without the Termux marker, preserve argv verbatim.
func FromArgv(goos, workingDirectory, termuxSelfExe string, argv []string) []string {
	if len(argv) < 2 {
		return nil
	}
	arguments := argv[1:]
	if goos == "android" && termuxSelfExe != "" && samePath(workingDirectory, termuxSelfExe, arguments[0]) {
		arguments = arguments[1:]
	}
	return arguments
}

func samePath(workingDirectory, left, right string) bool {
	canonical := func(path string) string {
		if filepath.IsAbs(path) || workingDirectory == "" {
			return filepath.Clean(path)
		}
		return filepath.Clean(filepath.Join(workingDirectory, path))
	}
	return canonical(left) == canonical(right)
}

func Current() []string {
	workingDirectory, _ := os.Getwd()
	return FromArgv(runtime.GOOS, workingDirectory, os.Getenv(termuxSelfExeEnvironment), os.Args)
}
