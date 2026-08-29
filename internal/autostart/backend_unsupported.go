//go:build !windows && !linux && !android

package autostart

import "errors"

func currentPaths() (platformPaths, error) {
	return platformPaths{}, errors.New("autostart is supported on Windows, Linux with systemd, and Android under Termux")
}

func installPlatform(platformPaths, Config) (Result, error) {
	return Result{}, errors.New("autostart is not supported on this platform")
}

func statusPlatform(platformPaths) (Result, error) {
	return Result{}, errors.New("autostart is not supported on this platform")
}

func uninstallPlatform(platformPaths) (Result, error) {
	return Result{}, errors.New("autostart is not supported on this platform")
}
