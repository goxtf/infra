//go:build linux

package userfaultfd

// https://docs.kernel.org/admin-guide/mm/userfaultfd.html
// https://man7.org/linux/man-pages/man2/userfaultfd.2.html
// https://github.com/torvalds/linux/blob/master/fs/userfaultfd.c
// https://github.com/loopholelabs/userfaultfd-go/blob/main/pkg/constants/cgo.go

/*
#include <sys/syscall.h>
#include <fcntl.h>
#include <linux/userfaultfd.h>
#include <sys/ioctl.h>

struct uffd_pagefault {
	__u64 flags;
	__u64 address;
	__u32 ptid;
};

#ifndef UFFD_FEATURE_WP_ASYNC
#define UFFD_FEATURE_WP_ASYNC (1 << 15)
#endif

struct uffd_remove {
	__u64 start;
	__u64 end;
};

#ifndef UFFD_FEATURE_MINOR_HUGETLBFS
#define UFFD_FEATURE_MINOR_HUGETLBFS (1 << 9)
#endif

#ifndef UFFD_FEATURE_MINOR_SHMEM
#define UFFD_FEATURE_MINOR_SHMEM (1 << 10)
#endif

#ifndef UFFDIO_REGISTER_MODE_MINOR
#define UFFDIO_REGISTER_MODE_MINOR ((__u64)1 << 1)
#endif

#ifndef UFFDIO_CONTINUE
#define _UFFDIO_CONTINUE 0x07
#define UFFDIO_CONTINUE _IOWR(0xAA, _UFFDIO_CONTINUE, struct uffdio_continue)

struct uffdio_continue {
	struct uffdio_range range;
	__u64 mode;
	__s64 mapped;
};
#endif
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	NR_userfaultfd = C.__NR_userfaultfd

	UFFD_API = C.UFFD_API

	UFFD_EVENT_PAGEFAULT = C.UFFD_EVENT_PAGEFAULT
	UFFD_EVENT_REMOVE    = C.UFFD_EVENT_REMOVE

	UFFDIO_REGISTER_MODE_MISSING = C.UFFDIO_REGISTER_MODE_MISSING
	UFFDIO_REGISTER_MODE_WP      = C.UFFDIO_REGISTER_MODE_WP
	UFFDIO_REGISTER_MODE_MINOR   = C.UFFDIO_REGISTER_MODE_MINOR

	UFFDIO_COPY_MODE_WP = C.UFFDIO_COPY_MODE_WP

	UFFDIO_WRITEPROTECT_MODE_WP = C.UFFDIO_WRITEPROTECT_MODE_WP

	UFFDIO_ZEROPAGE_MODE_DONTWAKE = C.UFFDIO_ZEROPAGE_MODE_DONTWAKE

	UFFDIO_API          = C.UFFDIO_API
	UFFDIO_REGISTER     = C.UFFDIO_REGISTER
	UFFDIO_UNREGISTER   = C.UFFDIO_UNREGISTER
	UFFDIO_COPY         = C.UFFDIO_COPY
	UFFDIO_ZEROPAGE     = C.UFFDIO_ZEROPAGE
	UFFDIO_WRITEPROTECT = C.UFFDIO_WRITEPROTECT
	UFFDIO_WAKE         = C.UFFDIO_WAKE
	UFFDIO_CONTINUE     = C.UFFDIO_CONTINUE

	UFFD_PAGEFAULT_FLAG_WRITE = C.UFFD_PAGEFAULT_FLAG_WRITE
	UFFD_PAGEFAULT_FLAG_MINOR = C.UFFD_PAGEFAULT_FLAG_MINOR
	UFFD_PAGEFAULT_FLAG_WP    = C.UFFD_PAGEFAULT_FLAG_WP

	UFFD_FEATURE_MISSING_HUGETLBFS = C.UFFD_FEATURE_MISSING_HUGETLBFS
	UFFD_FEATURE_EVENT_REMOVE      = C.UFFD_FEATURE_EVENT_REMOVE
	UFFD_FEATURE_WP_ASYNC          = C.UFFD_FEATURE_WP_ASYNC
	UFFD_FEATURE_MINOR_HUGETLBFS   = C.UFFD_FEATURE_MINOR_HUGETLBFS
	UFFD_FEATURE_MINOR_SHMEM       = C.UFFD_FEATURE_MINOR_SHMEM
)

type (
	CULong = C.ulonglong
	CUChar = C.uchar
	CLong  = C.longlong

	UffdMsg       = C.struct_uffd_msg
	UffdPagefault = C.struct_uffd_pagefault
	UffdRemove    = C.struct_uffd_remove

	UffdioAPI          = C.struct_uffdio_api
	UffdioRegister     = C.struct_uffdio_register
	UffdioRange        = C.struct_uffdio_range
	UffdioCopy         = C.struct_uffdio_copy
	UffdioZero         = C.struct_uffdio_zeropage
	UffdioWriteProtect = C.struct_uffdio_writeprotect
	UffdioContinue     = C.struct_uffdio_continue
)

func newUffdioAPI(api, features CULong) UffdioAPI {
	return UffdioAPI{
		api:      api,
		features: features,
	}
}

func newUffdioRange(start, length CULong) UffdioRange {
	return UffdioRange{
		start: start,
		len:   length,
	}
}

func newUffdioRegister(start, length, mode CULong) UffdioRegister {
	return UffdioRegister{
		_range: newUffdioRange(start, length),
		mode:   mode,
	}
}

func newUffdioCopy(b []byte, address CULong, pagesize CULong, mode CULong, bytesCopied CLong) UffdioCopy {
	return UffdioCopy{
		src:  CULong(uintptr(unsafe.Pointer(&b[0]))),
		dst:  address,
		len:  pagesize,
		mode: mode,
		copy: bytesCopied,
	}
}

func newUffdioZero(address, pagesize, mode CULong) UffdioZero {
	return UffdioZero{
		_range:   newUffdioRange(address, pagesize),
		mode:     mode,
		zeropage: 0,
	}
}

func newUffdioWriteProtect(address, pagesize, mode CULong) UffdioWriteProtect {
	return UffdioWriteProtect{
		_range: newUffdioRange(address, pagesize),
		mode:   mode,
	}
}

func newUffdioContinue(address, pagesize, mode CULong) UffdioContinue {
	return UffdioContinue{
		_range: newUffdioRange(address, pagesize),
		mode:   mode,
	}
}

func getMsgEvent(msg *UffdMsg) CUChar {
	return msg.event
}

func getMsgArg(msg *UffdMsg) [24]byte {
	return msg.arg
}

func getPagefaultAddress(pagefault *UffdPagefault) uintptr {
	return uintptr(pagefault.address)
}

// Fd is a helper type that wraps uffd fd.
type Fd uintptr

// copy requires UFFDIO_COPY_MODE_WP when both MISSING and WP tracking are active.
func (f Fd) copy(addr, pagesize uintptr, data []byte, mode CULong) error {
	cpy := newUffdioCopy(data, CULong(addr)&^CULong(pagesize-1), CULong(pagesize), mode, 0)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(f), UFFDIO_COPY, uintptr(unsafe.Pointer(&cpy))); errno != 0 {
		return errno
	}

	return classifyCopyResult(int64(cpy.copy), int64(pagesize))
}

// classifyCopyResult turns the UFFDIO_COPY cpy.copy field into a Go error.
// The kernel encodes a negated errno on failure (most commonly -EAGAIN when
// mmap_changing is set), and a short positive count when the copy was
// preempted mid-page (e.g. hugetlb). Both partial outcomes surface as EAGAIN;
// the caller defers the fault for retry on the next poll iteration (via the
// deferred queue + wakeup pipe), the kernel does not auto-redeliver.
func classifyCopyResult(bytesCopied, pagesize int64) error {
	if bytesCopied < 0 {
		return syscall.Errno(-bytesCopied)
	}

	if bytesCopied != pagesize {
		return syscall.EAGAIN
	}

	return nil
}

func (f Fd) zero(addr, pagesize uintptr, mode CULong) error {
	zero := newUffdioZero(CULong(addr)&^CULong(pagesize-1), CULong(pagesize), mode)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(f), UFFDIO_ZEROPAGE, uintptr(unsafe.Pointer(&zero))); errno != 0 {
		return errno
	}

	if zero.zeropage != CLong(pagesize) {
		return fmt.Errorf("UFFDIO_ZEROPAGE copied %d bytes, expected %d", zero.zeropage, pagesize)
	}

	return nil
}

func (f Fd) writeProtect(addr, pagesize uintptr, mode CULong) error {
	writeProtect := newUffdioWriteProtect(CULong(addr)&^CULong(pagesize-1), CULong(pagesize), mode)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(f), UFFDIO_WRITEPROTECT, uintptr(unsafe.Pointer(&writeProtect))); errno != 0 {
		return errno
	}

	return nil
}

// continue installs into the VMA a page that is already present in the
// underlying file (memfd) backing. Resolves MINOR faults zero-copy: the
// kernel only updates the page-table entry for this VMA, no memcpy.
// Requires the VMA to be file-backed (hugetlbfs or shmem) and the UFFD
// to have been opened with UFFD_FEATURE_MINOR_HUGETLBFS / _SHMEM.
func (f Fd) continueIoctl(addr, pagesize uintptr) error {
	c := newUffdioContinue(CULong(addr)&^CULong(pagesize-1), CULong(pagesize), 0)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(f), UFFDIO_CONTINUE, uintptr(unsafe.Pointer(&c))); errno != 0 {
		return errno
	}

	if c.mapped < 0 {
		return syscall.Errno(-c.mapped)
	}
	if c.mapped != CLong(pagesize) {
		return fmt.Errorf("UFFDIO_CONTINUE installed %d bytes, expected %d", c.mapped, pagesize)
	}

	return nil
}

func (f Fd) wake(addr, pagesize uintptr) error {
	uffdRange := newUffdioRange(CULong(addr)&^CULong(pagesize-1), CULong(pagesize))

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(f), UFFDIO_WAKE, uintptr(unsafe.Pointer(&uffdRange))); errno != 0 {
		return errno
	}

	return nil
}

func (f Fd) close() error {
	return syscall.Close(int(f))
}
