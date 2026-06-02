//go:build windows

package main

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Native Windows API metrics — no wmic.exe (which opens a console window
// every time it runs, causing cmd popups). Uses kernel32.dll for memory and
// pdh.dll for CPU percent via Performance Counters.

var (
	pdh   = syscall.NewLazyDLL("pdh.dll")
	psapi = syscall.NewLazyDLL("psapi.dll")

	procGlobalMemoryStatusEx        = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64              = kernel32.NewProc("GetTickCount64")
	procPdhOpenQuery                = pdh.NewProc("PdhOpenQuery")
	procPdhAddCounter               = pdh.NewProc("PdhAddCounterW")
	procPdhCollectQueryData         = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterValue = pdh.NewProc("PdhGetFormattedCounterValue")
	procPdhCloseQuery               = pdh.NewProc("PdhCloseQuery")
	procPdhRemoveCounter            = pdh.NewProc("PdhRemoveCounter")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhysical        uint64
	AvailablePhysical    uint64
	TotalPageFile        uint64
	AvailablePageFile    uint64
	TotalVirtual         uint64
	AvailableVirtual     uint64
	AvailExtendedVirtual uint64
}

const (
	pdhFmtDouble = 0x00000200
	pdhFmtNoCap  = 0x00000000
)

type pdhFmtCounterValue struct {
	CStatus     uint32
	_pad        [4]byte
	doubleValue float64
	longValue   int64
	largeValue  int64
	_pad2       [4]byte
}

// CPU tracker state: uses PdhTotalProcessorTime to compute %
type cpuTracker struct {
	mu      sync.Mutex
	query   uintptr
	counter uintptr
	ready   bool
}

var cpuTrack = &cpuTracker{}

func initCPUTracker() {
	cpuTrack.mu.Lock()
	defer cpuTrack.mu.Unlock()
	if cpuTrack.ready {
		return
	}
	// PdhOpenQuery(NULL, 0, &query)
	var query uintptr
	r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&query)))
	if r != 0 {
		return
	}
	// Counter path: \Processor Information(_Total)\% Processor Time
	// (we use the modern "Processor Information" since "Processor" is deprecated)
	counterPath, _ := syscall.UTF16PtrFromString(`\Processor Information(_Total)\% Processor Time`)
	var counter uintptr
	r, _, _ = procPdhAddCounter.Call(
		query,
		uintptr(unsafe.Pointer(counterPath)),
		0,
		uintptr(unsafe.Pointer(&counter)),
	)
	if r != 0 {
		// Try legacy \Processor(_Total)\% Processor Time
		counterPath2, _ := syscall.UTF16PtrFromString(`\Processor(_Total)\% Processor Time`)
		r, _, _ = procPdhAddCounter.Call(
			query,
			uintptr(unsafe.Pointer(counterPath2)),
			0,
			uintptr(unsafe.Pointer(&counter)),
		)
		if r != 0 {
			syscall.SyscallN(procPdhCloseQuery.Addr(), query)
			return
		}
	}
	cpuTrack.query = query
	cpuTrack.counter = counter
	cpuTrack.ready = true
	// Prime: collect once so the first call returns a real value
	syscall.SyscallN(procPdhCollectQueryData.Addr(), query)
}

func getNativeCPUPercent() float64 {
	cpuTrack.mu.Lock()
	defer cpuTrack.mu.Unlock()
	if !cpuTrack.ready {
		initCPUTracker()
		if !cpuTrack.ready {
			return 0
		}
	}
	r, _, _ := procPdhCollectQueryData.Call(cpuTrack.query)
	if r != 0 {
		return 0
	}
	var value pdhFmtCounterValue
	r, _, _ = procPdhGetFormattedCounterValue.Call(
		cpuTrack.counter,
		pdhFmtDouble|pdhFmtNoCap,
		0,
		uintptr(unsafe.Pointer(&value)),
	)
	if r != 0 {
		return 0
	}
	pct := value.doubleValue
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

func getNativeMemoryUsage() (usedMB float64, totalMB float64) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, 0
	}
	totalMB = float64(m.TotalPhysical) / 1024 / 1024
	usedMB = float64(m.TotalPhysical-m.AvailablePhysical) / 1024 / 1024
	return usedMB, totalMB
}

// NativeBootTimeMS returns system boot time in milliseconds since Unix epoch
// using GetTickCount64 (kernel32). No external process.
func nativeBootTimeMS() int64 {
	r, _, _ := procGetTickCount64.Call()
	if r == 0 {
		return 0
	}
	uptimeMS := int64(r)
	return time.Now().UnixMilli() - uptimeMS
}
