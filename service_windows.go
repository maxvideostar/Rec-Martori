//go:build windows

package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const svcName = "RecMartori"

func isService() bool {
	ok, _ := svc.IsWindowsService()
	return ok
}

type svcHandler struct{ cfg string }

func (h *svcHandler) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() { run(h.cfg, quit); close(done) }()
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				st <- svc.Status{State: svc.StopPending, WaitHint: 30000}
				close(quit)
				select {
				case <-done:
				case <-time.After(28 * time.Second):
				}
				return false, 0
			}
		case <-done:
			return false, 1 // s-a oprit singur → Windows îl repornește (recovery)
		}
	}
}

func runAsService(cfg string) {
	if err := svc.Run(svcName, &svcHandler{cfg}); err != nil {
		os.Exit(1)
	}
}

func serviceCmd(install bool, cfg string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("nu am acces la Service Manager — rulează ca Administrator (%v)", err)
	}
	defer m.Disconnect()

	if !install {
		s, err := m.OpenService(svcName)
		if err != nil {
			return fmt.Errorf("serviciul %s nu este instalat", svcName)
		}
		defer s.Close()
		if q, err := s.Query(); err == nil && q.State != svc.Stopped {
			fmt.Println("Opresc serviciul (închid înregistrările curat)...")
			s.Control(svc.Stop)
			for i := 0; i < 45; i++ {
				time.Sleep(time.Second)
				if q, err := s.Query(); err != nil || q.State == svc.Stopped {
					break
				}
			}
		}
		if err := s.Delete(); err != nil {
			return err
		}
		fmt.Println("Serviciul", svcName, "a fost dezinstalat.")
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if s, err := m.OpenService(svcName); err == nil {
		s.Close()
		return fmt.Errorf("serviciul %s există deja (dezinstalează-l întâi cu -uninstall)", svcName)
	}
	s, err := m.CreateService(svcName, exe, mgr.Config{
		DisplayName: "Rec-Martori",
		Description: "Înregistrare martori TV (FFmpeg) + interfață web",
		StartType:   mgr.StartAutomatic,
	}, "-config", cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	// dacă procesul cade, Windows îl repornește după 10 s (de fiecare dată)
	s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400)
	s.SetRecoveryActionsOnNonCrashFailures(true)
	if err := s.Start(); err != nil {
		return fmt.Errorf("instalat, dar nu a pornit: %v", err)
	}
	fmt.Println("Serviciul", svcName, "a fost instalat și pornit (pornire automată cu Windows).")
	fmt.Println("Config:", cfg)
	return nil
}
