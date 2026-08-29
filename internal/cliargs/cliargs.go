package cliargs

import (
	"os"
	"runtime"
)

// FromArgv returns command arguments without argv[0]. Go Android command-line
// binaries launched from Termux receive argv[0] twice, so remove the duplicate
// before handing arguments to flag.FlagSet.
func FromArgv(goos string, argv []string) []string {
	if len(argv) < 2 {
		return nil
	}
	arguments := argv[1:]
	if goos == "android" && len(arguments) > 0 && arguments[0] == argv[0] {
		arguments = arguments[1:]
	}
	return arguments
}

func Current() []string {
	return FromArgv(runtime.GOOS, os.Args)
}
