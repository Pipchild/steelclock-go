//go:build windows

package fps

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

var (
	modKernel32          = syscall.NewLazyDLL("kernel32.dll")
	procOpenFileMappingW = modKernel32.NewProc("OpenFileMappingW")
	procMapViewOfFile    = modKernel32.NewProc("MapViewOfFile")
	procUnmapViewOfFile  = modKernel32.NewProc("UnmapViewOfFile")
)

const (
	fileMapRead      = 0x0004
	rtssMappingName  = "RTSSSharedMemoryV2"
	rtssMaxProcesses = 256 // matches RTSS_SHARED_MEMORY::arrApp[256]

	// Header field byte offsets. Stable since RTSS shared memory v2.0 — the
	// header itself carries dwAppArrOffset/dwAppEntrySize/dwAppArrSize so app
	// entries never need to be located by a hardcoded struct size.
	// See RTSSSharedMemory.h (RTSS SDK) for the authoritative layout.
	offAppEntrySize               = 8
	offAppArrOffset               = 12
	offAppArrSize                 = 16
	offLastForegroundAppProcessID = 68 // valid for shared memory v2.16+; 0 if older/unset

	// RTSS_SHARED_MEMORY_APP_ENTRY field byte offsets, relative to the
	// entry's own base. These are the struct's leading fields, stable since
	// v2.0 regardless of how much trailing data newer RTSS versions add.
	entryOffProcessID = 0
	entryOffName      = 4
	entryOffNameLen   = 260 // MAX_PATH
	entryOffFrameTime = 280 // frame time in microseconds
)

// rtssReader reads current framerate from RTSS's (RivaTuner Statistics
// Server) shared memory segment. RTSS is the framerate-capture engine behind
// MSI Afterburner and is widely used by other on-screen-display tools as a
// stable, already-hooked source of per-process FPS without implementing our
// own DirectX/OpenGL/Vulkan present hooks.
type rtssReader struct {
	base uintptr
	mu   sync.Mutex
}

// newRTSSReader opens and maps the RTSS shared memory segment. Fails if RTSS
// isn't running — the caller should retry periodically, since RTSS may start
// later (e.g. when a game launches) or may not be needed until then.
func newRTSSReader() (*rtssReader, error) {
	namePtr, err := syscall.UTF16PtrFromString(rtssMappingName)
	if err != nil {
		return nil, err
	}
	h, _, _ := procOpenFileMappingW.Call(fileMapRead, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return nil, fmt.Errorf("RTSS shared memory not found (is RTSS/MSI Afterburner running?)")
	}
	handle := syscall.Handle(h)
	defer func() { _ = syscall.CloseHandle(handle) }() // MapViewOfFile keeps its own reference

	base, _, _ := procMapViewOfFile.Call(h, fileMapRead, 0, 0, 0)
	if base == 0 {
		return nil, fmt.Errorf("failed to map RTSS shared memory")
	}
	return &rtssReader{base: base}, nil
}

func readU32(base uintptr, off uintptr) uint32 {
	return *(*uint32)(unsafe.Pointer(base + off))
}

func readCString(base uintptr, maxLen int) string {
	b := make([]byte, 0, maxLen)
	for i := 0; i < maxLen; i++ {
		c := *(*byte)(unsafe.Pointer(base + uintptr(i)))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}

// GetFPS returns the current framerate and process name of RTSS's tracked
// foreground application. Returns (0, "", nil) when RTSS is running but no
// application is currently hooked and active — not an error condition.
func (r *rtssReader) GetFPS() (fps float64, processName string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appEntrySize := uintptr(readU32(r.base, offAppEntrySize))
	appArrOffset := uintptr(readU32(r.base, offAppArrOffset))
	appArrSize := readU32(r.base, offAppArrSize)
	if appArrSize > rtssMaxProcesses {
		appArrSize = rtssMaxProcesses
	}
	fgPID := readU32(r.base, offLastForegroundAppProcessID)

	if appEntrySize == 0 || appArrSize == 0 {
		return 0, "", nil
	}

	for i := uint32(0); i < appArrSize; i++ {
		entryBase := r.base + appArrOffset + uintptr(i)*appEntrySize
		pid := readU32(entryBase, entryOffProcessID)
		if pid == 0 {
			continue
		}
		// Prefer the foreground app when RTSS reports one (v2.16+); older
		// versions leave this at 0, so fall back to the first active entry.
		if fgPID != 0 && pid != fgPID {
			continue
		}
		frameTime := readU32(entryBase, entryOffFrameTime)
		if frameTime == 0 {
			continue
		}
		name := readCString(entryBase+entryOffName, entryOffNameLen)
		return 1000000.0 / float64(frameTime), name, nil
	}

	return 0, "", nil
}

// Close unmaps the shared memory segment.
func (r *rtssReader) Close() {
	if r.base != 0 {
		_, _, _ = procUnmapViewOfFile.Call(r.base)
		r.base = 0
	}
}

// newReader opens the RTSS shared memory segment as a Reader.
func newReader() (Reader, error) {
	return newRTSSReader()
}
