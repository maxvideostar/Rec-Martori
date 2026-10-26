//go:build !windows

package main

import "errors"

func isService() bool         { return false }
func runAsService(cfg string) {}
func serviceCmd(install bool, cfg string) error {
	return errors.New("serviciul Windows e disponibil doar pe Windows — pe Linux folosește systemd")
}

func serviceState() (bool, string)                  { return false, "" }
func isAdmin() bool                                 { return false }
func elevate(args string) error                     { return errors.New("doar pe Windows") }
func serviceHelper(install bool, cfg string, p int) {}
func selfUninstall() error                          { return errors.New("doar pe Windows") }
