//go:build linux

package userfaultfd

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const fourKiB = 4 * 1024

// TestContinue_MemfdHugetlb exercises both halves of the UFFDIO_CONTINUE
// flow we need on top of a hugetlb-backed, MAP_SHARED memfd:
//
//   - "prefetch_minor": memfd is fully populated (in 4 KiB strides — the
//     production dedup-storage pattern) before any access. Faults arrive as
//     MINOR; UFFDIO_CONTINUE resolves them zero-copy.
//
//   - "cold_missing":  memfd is sparse. First access raises MISSING; the
//     handler memcpys bytes into the memfd via a writer mmap, then calls
//     UFFDIO_CONTINUE — which works on a MISSING fault as well, because by
//     that time the hugepage is in the memfd file backing.
//
// Together these cover the prefetch and cold-fault production paths.
func TestContinue_MemfdHugetlb(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("requires root for userfaultfd and hugetlb reservation")
	}

	t.Run("prefetch_minor", func(t *testing.T) {
		runContinueHugetlb(t, true /* prefill */)
	})
	t.Run("cold_missing", func(t *testing.T) {
		runContinueHugetlb(t, false /* prefill */)
	})
}

func runContinueHugetlb(t *testing.T, prefill bool) {
	t.Helper()

	const numPages = 2
	pageSize := int64(header.HugepageSize)
	size := pageSize * numPages

	// Reserve enough 2 MiB hugepages for this test; restore on cleanup.
	reserveHugePages(t, numPages+1)

	memfd, err := unix.MemfdCreate("uffd-continue-test", unix.MFD_HUGETLB|unix.MFD_HUGE_2MB)
	require.NoError(t, err)
	t.Cleanup(func() { syscall.Close(memfd) })
	require.NoError(t, unix.Ftruncate(memfd, size))

	// Build the bytes we expect the reader to observe — same shape for
	// both subtests. In the prefill case we lay them down up-front via a
	// writer mmap; in the cold case the handler does the same memcpy
	// during fault resolution.
	expected := make([]byte, size)
	for off := int64(0); off < size; off += fourKiB {
		fill := byte((off / fourKiB) + 1)
		for i := int64(0); i < fourKiB; i++ {
			expected[off+i] = fill
		}
	}

	if prefill {
		writer, err := unix.Mmap(memfd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		require.NoError(t, err)
		copy(writer, expected)
		require.NoError(t, unix.Munmap(writer))
	}

	// Reader VMA — what UFFD is registered on.
	mapped, err := unix.Mmap(memfd, 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	require.NoError(t, err)
	t.Cleanup(func() { unix.Munmap(mapped) })

	uffd, err := newFd(syscall.O_CLOEXEC | syscall.O_NONBLOCK)
	require.NoError(t, err)
	t.Cleanup(func() { uffd.close() })
	require.NoError(t, configureContinueAPI(uffd))

	base := uintptr(unsafe.Pointer(&mapped[0]))
	require.NoError(t, register(uffd, base, uint64(size),
		UFFDIO_REGISTER_MODE_MISSING|UFFDIO_REGISTER_MODE_MINOR))
	t.Cleanup(func() { _ = unregister(uffd, base, uint64(size)) })

	// Handler. On a MINOR fault we go straight to CONTINUE. On a MISSING
	// fault we first memcpy this hugepage's bytes through a writer mmap
	// to populate the memfd file backing, then CONTINUE installs it in
	// the reader VMA — same ioctl, both paths.
	var (
		minorFaults   int
		missingFaults int
		handlerErr    error
		handlerWG     sync.WaitGroup
	)
	stop := make(chan struct{})
	handlerWG.Add(1)
	go func() {
		defer handlerWG.Done()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		for {
			select {
			case <-stop:
				return
			default:
			}

			msg, ok, err := readUffdMsg(uffd)
			if err != nil {
				handlerErr = err
				return
			}
			if !ok {
				continue
			}
			if getMsgEvent(&msg) != UFFD_EVENT_PAGEFAULT {
				continue
			}
			arg := getMsgArg(&msg)
			pf := (*UffdPagefault)(unsafe.Pointer(&arg[0]))
			faultAddr := getPagefaultAddress(pf)

			isMinor := pf.flags&UFFD_PAGEFAULT_FLAG_MINOR != 0
			if isMinor {
				minorFaults++
			} else {
				missingFaults++
				// Cold fault: populate the hugepage in the memfd
				// file backing before installing it. Mirrors what
				// the production handler will do (chunker memcpy
				// into the memfd at the right offset).
				offset := int64(faultAddr) - int64(base)
				offset &= ^(pageSize - 1)
				if err := populateHugepage(memfd, offset, expected[offset:offset+pageSize]); err != nil {
					handlerErr = err
					return
				}
			}

			if err := uffd.continueIoctl(faultAddr, uintptr(pageSize)); err != nil {
				handlerErr = err
				return
			}
		}
	}()
	defer func() {
		close(stop)
		handlerWG.Wait()
		require.NoError(t, handlerErr)
		if prefill {
			require.Equal(t, numPages, minorFaults, "every page should fault as MINOR when memfd is prefilled")
			require.Equal(t, 0, missingFaults)
		} else {
			require.Equal(t, numPages, missingFaults, "every page should fault as MISSING when memfd is sparse")
			require.Equal(t, 0, minorFaults)
		}
	}()

	got := make([]byte, size)
	readDone := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := unix.Madvise(mapped, unix.MADV_POPULATE_READ); err != nil {
			readDone <- err
			return
		}
		copy(got, mapped)
		readDone <- nil
	}()

	select {
	case err := <-readDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for faulting goroutine")
	}

	require.Equal(t, expected, got, "bytes installed by UFFDIO_CONTINUE must equal what we wrote into the memfd")
}

// populateHugepage writes one 2 MiB hugepage's worth of bytes into the memfd
// at offset, via a fresh writer mmap. The mmap is short-lived; only the
// memfd file backing matters — the reader VMA picks the page up via
// UFFDIO_CONTINUE.
func populateHugepage(memfd int, offset int64, data []byte) error {
	w, err := unix.Mmap(memfd, offset, len(data), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("writer mmap: %w", err)
	}
	copy(w, data)
	if err := unix.Munmap(w); err != nil {
		return fmt.Errorf("writer munmap: %w", err)
	}
	return nil
}

// reserveHugePages bumps /proc/sys/vm/nr_hugepages to at least n if we
// need more, and restores the previous value at test end.
func reserveHugePages(t *testing.T, n int) {
	t.Helper()
	prev := readNrHugePages(t)
	if prev >= n {
		return
	}
	writeNrHugePages(t, n)
	if got := readNrHugePages(t); got < n {
		t.Fatalf("could not reserve %d hugepages (got %d) — host may be too fragmented", n, got)
	}
	t.Cleanup(func() { writeNrHugePages(t, prev) })
}

func readNrHugePages(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/vm/nr_hugepages")
	require.NoError(t, err)
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	require.NoError(t, err)
	return v
}

func writeNrHugePages(t *testing.T, n int) {
	t.Helper()
	require.NoError(t, os.WriteFile("/proc/sys/vm/nr_hugepages", []byte(strconv.Itoa(n)+"\n"), 0o644))
}

func configureContinueAPI(f Fd) error {
	api := newUffdioAPI(UFFD_API, UFFD_FEATURE_MINOR_HUGETLBFS)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(f), UFFDIO_API, uintptr(unsafe.Pointer(&api))); errno != 0 {
		return errno
	}
	return nil
}

func readUffdMsg(f Fd) (UffdMsg, bool, error) {
	var msg UffdMsg
	n, _, errno := syscall.Syscall(syscall.SYS_READ, uintptr(f), uintptr(unsafe.Pointer(&msg)), unsafe.Sizeof(msg))
	if errno == syscall.EAGAIN {
		time.Sleep(100 * time.Microsecond)
		return msg, false, nil
	}
	if errno != 0 {
		return msg, false, errno
	}
	if int(n) != int(unsafe.Sizeof(msg)) {
		return msg, false, errors.New("short uffd read")
	}
	return msg, true, nil
}
