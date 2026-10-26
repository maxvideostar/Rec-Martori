//go:build !windows

package main

import "errors"

func isService() bool         { return false }
func runAsService(cfg string) {}
func serviceCmd(install bool, cfg string) error {
	return errors.New("serviciul Windows e disponibil doar pe Windows — pe Linux folosește systemd")
}
