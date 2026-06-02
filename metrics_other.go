//go:build !windows

package main

func getNativeCPUPercent() float64 {
	return 0
}

func getNativeMemoryUsage() (usedMB float64, totalMB float64) {
	return 0, 0
}

func nativeBootTimeMS() int64 {
	return 0
}
