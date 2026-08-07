//go:build windows

package platform

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procstampKernel32     = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcessPS     = procstampKernel32.NewProc("OpenProcess")
	procGetProcessTimes   = procstampKernel32.NewProc("GetProcessTimes")
	procQueryFullImageW   = procstampKernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandlePS     = procstampKernel32.NewProc("CloseHandle")
	processQueryLimitedPS = uintptr(0x1000) // PROCESS_QUERY_LIMITED_INFORMATION
)

func processStamp(pid int) string {
	h, _, _ := procOpenProcessPS.Call(processQueryLimitedPS, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandlePS.Call(h)

	var buf [syscall.MAX_PATH]uint16
	size := uint32(len(buf))
	r, _, _ := procQueryFullImageW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return ""
	}
	name := strings.ToLower(filepath.Base(syscall.UTF16ToString(buf[:size])))

	var creation, exit, kernel, user syscall.Filetime
	r, _, _ = procGetProcessTimes.Call(h,
		uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return ""
	}
	return fmt.Sprintf("%s|%d.%d", name, creation.HighDateTime, creation.LowDateTime)
}
