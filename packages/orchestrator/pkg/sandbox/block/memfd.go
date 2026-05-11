//go:build linux

package block

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

type Memfd struct {
	fd    int
	bytes []byte
}

func NewFromFd(fd int) (*Memfd, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)

		return nil, fmt.Errorf("fstat memfd: %w", err)
	}

	bytes, err := unix.Mmap(fd, 0, int(st.Size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(fd)

		return nil, fmt.Errorf("mmap memfd: %w", err)
	}

	return &Memfd{fd: fd, bytes: bytes}, nil
}

// Bytes returns the read-only mmap of the memfd. Valid until Close.
func (m *Memfd) Bytes() []byte {
	return m.bytes
}

func (m *Memfd) Size() int64 {
	return int64(len(m.bytes))
}

// Close is idempotent.
func (m *Memfd) Close() error {
	var err error
	if m.bytes != nil {
		if e := unix.Munmap(m.bytes); e != nil {
			err = fmt.Errorf("munmap memfd: %w", e)
		}
		m.bytes = nil
	}
	if m.fd >= 0 {
		if e := syscall.Close(m.fd); e != nil {
			err = errors.Join(err, fmt.Errorf("close memfd: %w", e))
		}
		m.fd = -1
	}

	return err
}

type memfdSource struct {
	memfd   *Memfd
	entries []memfdRange
}

type memfdRange struct {
	cacheStart int64
	srcStart   int64
	size       int64
}

func newMemfdSource(memfd *Memfd, ranges []Range) *memfdSource {
	entries := make([]memfdRange, len(ranges))
	var cacheOff int64
	for i, r := range ranges {
		entries[i] = memfdRange{cacheStart: cacheOff, srcStart: r.Start, size: r.Size}
		cacheOff += r.Size
	}

	return &memfdSource{memfd: memfd, entries: entries}
}

func (s *memfdSource) findEntry(cacheOff int64) int {
	lo, hi := 0, len(s.entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if s.entries[mid].cacheStart > cacheOff {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	i := lo - 1
	if i < 0 {
		return -1
	}
	e := s.entries[i]
	if cacheOff >= e.cacheStart+e.size {
		return -1
	}

	return i
}

func (s *memfdSource) readAt(b []byte, cacheOff int64) int {
	src := s.memfd.Bytes()
	n := 0
	for n < len(b) {
		i := s.findEntry(cacheOff + int64(n))
		if i < 0 {
			return n
		}
		e := s.entries[i]
		offsetInEntry := cacheOff + int64(n) - e.cacheStart
		toCopy := min(int64(len(b)-n), e.size-offsetInEntry)
		copy(b[n:n+int(toCopy)], src[e.srcStart+offsetInEntry:e.srcStart+offsetInEntry+toCopy])
		n += int(toCopy)
	}

	return n
}

// MemfdCache wraps a *Cache that is being populated from a memfd in the
// background. Reads are served directly from the memfd until the copy
// completes; afterwards the wrapper just delegates to the underlying Cache and
// the memfd is closed.
type MemfdCache struct {
	cache *Cache

	mu     sync.RWMutex // guards src
	src    *memfdSource // nil once the background copy has completed
	cancel context.CancelFunc
	done   chan struct{}
	err    atomic.Pointer[error]
}

// NewCacheFromMemfd creates a Cache backed by an in-flight copy from the given
// memfd. The returned wrapper takes ownership of memfd: callers must Close the
// wrapper (which also closes the memfd).
func NewCacheFromMemfd(
	ctx context.Context,
	blockSize int64,
	filePath string,
	memfd *Memfd,
	ranges []Range,
) (*MemfdCache, error) {
	size := GetSize(ranges)

	cache, err := NewCache(size, blockSize, filePath, false)
	if err != nil {
		if closeErr := memfd.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}

		return nil, err
	}

	if size == 0 {
		if closeErr := memfd.Close(); closeErr != nil {
			cache.Close()

			return nil, fmt.Errorf("close memfd: %w", closeErr)
		}

		return &MemfdCache{cache: cache}, nil
	}

	copyCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m := &MemfdCache{
		cache:  cache,
		src:    newMemfdSource(memfd, ranges),
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go m.runCopy(copyCtx, ranges)

	return m, nil
}

func (m *MemfdCache) runCopy(ctx context.Context, ranges []Range) {
	defer close(m.done)

	err := m.copyFromMemfd(ctx, ranges)
	if err != nil {
		m.err.Store(&err)
	}

	m.mu.Lock()
	src := m.src
	m.src = nil
	m.mu.Unlock()

	if closeErr := src.memfd.Close(); closeErr != nil {
		joined := errors.Join(err, fmt.Errorf("close memfd: %w", closeErr))
		m.err.Store(&joined)
	}
}

func (m *MemfdCache) copyFromMemfd(ctx context.Context, ranges []Range) error {
	src := m.src.memfd.Bytes()

	var cacheOff int64
	for _, r := range ranges {
		rangeStart := cacheOff
		end := r.Start + r.Size

		for srcOff := r.Start; srcOff < end; srcOff += m.cache.blockSize {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			n := min(m.cache.blockSize, end-srcOff)
			copy((*m.cache.mmap)[cacheOff:cacheOff+n], src[srcOff:srcOff+n])
			cacheOff += n
		}

		m.cache.setIsCached(rangeStart, r.Size)
	}

	return nil
}

// Wait blocks until the background copy completes (or ctx is cancelled), and
// returns any error that occurred.
func (m *MemfdCache) Wait(ctx context.Context) error {
	if m.done == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
	}
	if errPtr := m.err.Load(); errPtr != nil {
		return *errPtr
	}

	return nil
}

func (m *MemfdCache) ReadAt(b []byte, off int64) (int, error) {
	m.mu.RLock()
	if m.src != nil {
		defer m.mu.RUnlock()

		return m.src.readAt(b, off), nil
	}
	m.mu.RUnlock()

	return m.cache.ReadAt(b, off)
}

// Slice returns BytesNotAvailableError while the background copy is in
// flight: the memfd-backed slice would outlive the RLock and could be
// Munmap'd asynchronously by runCopy. Callers should fall back to ReadAt
// (which copies into the caller's buffer) or Wait first.
func (m *MemfdCache) Slice(off, length int64) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.src != nil {
		return nil, BytesNotAvailableError{}
	}

	return m.cache.Slice(off, length)
}

func (m *MemfdCache) Close() error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}

	return m.cache.Close()
}

func (m *MemfdCache) Size() (int64, error)     { return m.cache.Size() }
func (m *MemfdCache) FileSize() (int64, error) { return m.cache.FileSize() }
func (m *MemfdCache) BlockSize() int64         { return m.cache.BlockSize() }
func (m *MemfdCache) Path() string             { return m.cache.Path() }
