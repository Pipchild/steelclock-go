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

	modUser32                    = syscall.NewLazyDLL("user32.dll")
	procGetForegroundWindow      = modUser32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = modUser32.NewProc("GetWindowThreadProcessId")
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
	entryOffTime0     = 268 // start of the once-per-second measurement period (ms, GetTickCount-style)
	entryOffTime1     = 272 // end of that measurement period (ms)
	entryOffFrames    = 276 // frames rendered during (Time1 - Time0)
	entryOffFrameTime = 280 // most recent single-frame time, in microseconds
)

// getForegroundProcessID returns the process ID owning the current OS
// foreground window. This is more reliable than RTSS's own
// dwLastForegroundAppProcessID field, which can be stale right after RTSS
// (re)starts — e.g. after an update — until the next focus change.
func getForegroundProcessID() uint32 {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	_, _, _ = procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

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

// GetFPS returns the current framerate and process name of the active
// foreground game. Returns (0, "", nil) when RTSS is running but nothing
// relevant is currently hooked and rendering — not an error condition.
func (r *rtssReader) GetFPS() (fps float64, processName string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appEntrySize := uintptr(readU32(r.base, offAppEntrySize))
	appArrOffset := uintptr(readU32(r.base, offAppArrOffset))
	appArrSize := readU32(r.base, offAppArrSize)
	if appArrSize > rtssMaxProcesses {
		appArrSize = rtssMaxProcesses
	}
	rtssFgPID := readU32(r.base, offLastForegroundAppProcessID)
	osFgPID := getForegroundProcessID()

	if appEntrySize == 0 || appArrSize == 0 {
		return 0, "", nil
	}

	var (
		osMatchFPS, rtssMatchFPS, latestFPS       float64
		osMatchName, rtssMatchName, latestName    string
		osMatchFound, rtssMatchFound, latestFound bool
		latestTime1                               uint32
	)

	for i := uint32(0); i < appArrSize; i++ {
		entryBase := r.base + appArrOffset + uintptr(i)*appEntrySize
		pid := readU32(entryBase, entryOffProcessID)
		if pid == 0 {
			continue
		}
		// dwFrameTime (a single frame's time) is only used here as an "is
		// this entry actively rendering right now" signal — it's noisy
		// frame-to-frame. The displayed value uses the smoothed once-per-
		// second dwFrames/(Time1-Time0) window RTSS itself documents for
		// this purpose, matching what RTSS's own OSD and similar overlay
		// tools (Steam, etc.) show rather than a single-frame spike.
		if readU32(entryBase, entryOffFrameTime) == 0 {
			continue
		}
		time0 := readU32(entryBase, entryOffTime0)
		time1 := readU32(entryBase, entryOffTime1)
		frames := readU32(entryBase, entryOffFrames)
		if time0 == 0 || time1 <= time0 {
			continue
		}
		name := readCString(entryBase+entryOffName, entryOffNameLen)
		entryFPS := 1000.0 * float64(frames) / float64(time1-time0)

		if osFgPID != 0 && pid == osFgPID {
			osMatchFPS, osMatchName, osMatchFound = entryFPS, name, true
		}
		if rtssFgPID != 0 && pid == rtssFgPID {
			rtssMatchFPS, rtssMatchName, rtssMatchFound = entryFPS, name, true
		}
		if !latestFound || time1 > latestTime1 {
			latestFPS, latestName, latestTime1, latestFound = entryFPS, name, time1, true
		}
	}

	// Prefer matching the OS's actual foreground window — more reliable than
	// RTSS's own tracking, which can be stale right after RTSS (re)starts
	// (e.g. after an update) until the next focus change. Fall back to RTSS's
	// own foreground field, then to whichever hooked app rendered most
	// recently, rather than an arbitrary array-order pick.
	switch {
	case osMatchFound:
		return osMatchFPS, osMatchName, nil
	case rtssMatchFound:
		return rtssMatchFPS, rtssMatchName, nil
	case latestFound:
		return latestFPS, latestName, nil
	default:
		return 0, "", nil
	}
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
