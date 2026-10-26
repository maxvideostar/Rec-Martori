//go:build windows

package main

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

var (
	k32                      = syscall.NewLazyDLL("kernel32.dll")
	pGetDiskFreeSpaceExW     = k32.NewProc("GetDiskFreeSpaceExW")
	pCreateJobObjectW        = k32.NewProc("CreateJobObjectW")
	pSetInformationJobObject = k32.NewProc("SetInformationJobObject")
	pAssignProcessToJob      = k32.NewProc("AssignProcessToJobObject")
	pOpenProcess             = k32.NewProc("OpenProcess")

	jobOnce   sync.Once
	jobHandle uintptr
)

// ffmpeg rulează fără fereastră de consolă
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

type ioCounters struct{ a, b, c, d, e, f uint64 }
type basicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}
type extLimit struct {
	Basic                 basicLimit
	Io                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// Job Object: dacă aplicația e închisă/omorâtă, ffmpeg moare odată cu ea (fără înregistrări "orfane" duplicate)
func attachJob(cmd *exec.Cmd) {
	jobOnce.Do(func() {
		h, _, _ := pCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return
		}
		var info extLimit
		info.Basic.LimitFlags = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		r, _, _ := pSetInformationJobObject.Call(h, 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if r != 0 {
			jobHandle = h
		}
	})
	if jobHandle == 0 || cmd.Process == nil {
		return
	}
	ph, _, _ := pOpenProcess.Call(0x0100|0x0001, 0, uintptr(cmd.Process.Pid)) // SET_QUOTA | TERMINATE
	if ph == 0 {
		return
	}
	pAssignProcessToJob.Call(jobHandle, ph)
	syscall.CloseHandle(syscall.Handle(ph))
}

func diskUsage(path string) (free, total uint64, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, tot, tfree uint64
	r, _, e := pGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&tot)), uintptr(unsafe.Pointer(&tfree)))
	if r == 0 {
		return 0, 0, e
	}
	return avail, tot, nil
}
