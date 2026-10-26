//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
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
	go func() { run(h.cfg, quit, func() {}); close(done) }()
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
		defer s.Close()
		if q, err := s.Query(); err == nil && q.State == svc.Running {
			return fmt.Errorf("serviciul %s există deja și rulează", svcName)
		}
		if err := s.Start(); err != nil {
			return fmt.Errorf("serviciul există, dar nu a pornit: %v", err)
		}
		fmt.Println("Serviciul", svcName, "exista deja — l-am pornit.")
		return nil
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

// ── folosite de interfața web ──

func serviceState() (installed bool, state string) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, ""
	}
	defer windows.CloseServiceHandle(h)
	name, _ := windows.UTF16PtrFromString(svcName)
	sh, err := windows.OpenService(h, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false, ""
	}
	defer windows.CloseServiceHandle(sh)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(sh, &st); err != nil {
		return true, "unknown"
	}
	switch st.CurrentState {
	case windows.SERVICE_RUNNING:
		return true, "running"
	case windows.SERVICE_STOPPED:
		return true, "stopped"
	case windows.SERVICE_START_PENDING:
		return true, "starting"
	case windows.SERVICE_STOP_PENDING:
		return true, "stopping"
	}
	return true, "other"
}

func isAdmin() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// pornește o copie a exe-ului cu drepturi de Administrator (prompt UAC pe desktopul PC-ului)
func elevate(args string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	par, _ := windows.UTF16PtrFromString(args)
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))
	return windows.ShellExecute(0, verb, file, par, dir, windows.SW_HIDE)
}

// rulat elevat din interfață: așteaptă închiderea instanței din consolă, apoi instalează/dezinstalează
func serviceHelper(install bool, cfg string, waitPid int) {
	if waitPid > 0 {
		if h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(waitPid)); err == nil {
			windows.WaitForSingleObject(h, 60000)
			windows.CloseHandle(h)
		}
		time.Sleep(time.Second)
	}
	err := serviceCmd(install, cfg)
	res := filepath.Join(filepath.Dir(cfg), "service-result.txt")
	stamp := time.Now().Format("2006-01-02 15:04:05")
	if err != nil {
		os.WriteFile(res, []byte(stamp+" EROARE: "+err.Error()), 0o644)
		if install && waitPid > 0 {
			// instalarea a eșuat → repornesc aplicația în consolă ca înregistrările să nu stea
			exe, _ := os.Executable()
			c := exec.Command(exe, "-config", cfg)
			c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010} // CREATE_NEW_CONSOLE
			c.Start()
		}
		return
	}
	if install {
		os.WriteFile(res, []byte(stamp+" OK: instalat și pornit ca serviciu"), 0o644)
	}
}

// rulat chiar din serviciu: se marchează pentru ștergere și se oprește singur
func selfUninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	s, err := m.OpenService(svcName)
	if err != nil {
		m.Disconnect()
		return err
	}
	if err := s.Delete(); err != nil {
		s.Close()
		m.Disconnect()
		return err
	}
	go func() {
		time.Sleep(time.Second)
		s.Control(svc.Stop)
		s.Close()
		m.Disconnect()
	}()
	return nil
}
