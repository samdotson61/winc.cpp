//go:build windows

package server

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows has no parent-death signal: if winc is killed hard (Task Manager,
// console window closed), plain children would keep running and hold the GPU.
// A Job Object with KILL_ON_JOB_CLOSE fixes that -- the OS closes our job
// handle when winc dies, and every assigned child is terminated with it.
//
// Everything here is BEST-EFFORT: any failure falls back to pre-1.5 behavior
// (children are still stopped by winc's own Stop() on a normal exit). winc
// never touches processes it didn't start; the job only ever contains our
// own children.

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject  = kernel32.NewProc("CreateJobObjectW")
	procSetInfoJobObject = kernel32.NewProc("SetInformationJobObject")
	procAssignProcToJob  = kernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess      = kernel32.NewProc("OpenProcess")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

// Mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION.
type jobBasicLimitInfo struct {
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

// Mirrors IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// Mirrors JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobExtendedLimitInfo struct {
	BasicLimitInformation jobBasicLimitInfo
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	jobOnce   sync.Once
	jobHandle uintptr // 0 = unavailable; held open for winc's lifetime, closed by the OS at exit
)

// initJob creates the process-lifetime job object once.
func initJob() {
	h, _, _ := procCreateJobObject.Call(0, 0)
	if h == 0 {
		return
	}
	var info jobExtendedLimitInfo
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, _ := procSetInfoJobObject.Call(h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return
	}
	jobHandle = h
}

// configureChild is a no-op on Windows (assignment happens after Start).
func configureChild(c *exec.Cmd) {}

// addToJob ties a started child's lifetime to winc's. Best-effort.
func addToJob(c *exec.Cmd) {
	if c == nil || c.Process == nil {
		return
	}
	jobOnce.Do(initJob)
	if jobHandle == 0 {
		return
	}
	h, _, _ := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(c.Process.Pid))
	if h == 0 {
		return
	}
	_, _, _ = procAssignProcToJob.Call(jobHandle, h)
	_ = syscall.CloseHandle(syscall.Handle(h))
}
