//go:build linux

package block

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// newTestMemfd creates an anonymous memfd of `size` bytes, fills it with
// random data, and returns the fd plus the data that was written.
// The caller is responsible for closing the fd (typically by handing it to
// NewFromFd / NewCacheFromMemfd, which closes it).
func newTestMemfd(t *testing.T, size int64) (fd int, data []byte) {
	t.Helper()

	fd, err := unix.MemfdCreate("test", 0)
	require.NoError(t, err)
	require.NoError(t, unix.Ftruncate(fd, size))

	data = make([]byte, size)
	_, err = rand.Read(data)
	require.NoError(t, err)

	_, err = syscall.Pwrite(fd, data, 0)
	require.NoError(t, err)

	return fd, data
}

// --- Memfd ---------------------------------------------------------------

func TestMemfd_SliceFullRange(t *testing.T) {
	t.Parallel()

	const size = 4 * header.PageSize
	fd, data := newTestMemfd(t, size)

	m := NewFromFd(fd, size)
	t.Cleanup(func() { _ = m.Close() })

	got, err := m.Slice(0, size)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestMemfd_SliceMultipleViewsShareMmap(t *testing.T) {
	t.Parallel()

	// ensureMapped uses sync.Once; repeated Slice calls must reuse the same
	// mapping and return mutually consistent views of the same bytes.
	const size = 4 * header.PageSize
	fd, data := newTestMemfd(t, size)

	m := NewFromFd(fd, size)
	t.Cleanup(func() { _ = m.Close() })

	first, err := m.Slice(0, header.PageSize)
	require.NoError(t, err)
	require.Equal(t, data[:header.PageSize], first)

	// Slice the tail. Must also succeed (mmap reused, no second mmap call).
	second, err := m.Slice(header.PageSize, 3*header.PageSize)
	require.NoError(t, err)
	require.Equal(t, data[header.PageSize:], second)

	// Overlapping views point at the same backing memory.
	third, err := m.Slice(0, size)
	require.NoError(t, err)
	require.Equal(t, &first[0], &third[0], "overlapping Slice views must share backing memory")
}

func TestMemfd_SliceOutOfBounds(t *testing.T) {
	t.Parallel()

	const size = 4 * header.PageSize
	fd, _ := newTestMemfd(t, size)

	m := NewFromFd(fd, size)
	t.Cleanup(func() { _ = m.Close() })

	cases := []struct {
		name     string
		off, len int64
	}{
		{"negative offset", -1, header.PageSize},
		{"offset at size", size, header.PageSize},
		{"offset past size", size + 1, header.PageSize},
		{"length spills past end", size - header.PageSize + 1, header.PageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := m.Slice(tc.off, tc.len)
			require.Error(t, err)
		})
	}
}

func TestMemfd_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	const size = 2 * header.PageSize
	fd, _ := newTestMemfd(t, size)

	m := NewFromFd(fd, size)

	// Force the lazy mmap so Close has both mmap + fd to release.
	_, err := m.Slice(0, header.PageSize)
	require.NoError(t, err)

	require.NoError(t, m.Close())
	// Second close must not panic and must not return an error for
	// already-released resources — both fields are nil/-1 by now.
	require.NoError(t, m.Close())
}

func TestMemfd_CloseWithoutMmap(t *testing.T) {
	t.Parallel()

	// If Slice is never called, Close must still close the fd — and not
	// attempt Munmap on a nil mapping.
	const size = 2 * header.PageSize
	fd, _ := newTestMemfd(t, size)

	m := NewFromFd(fd, size)
	require.NoError(t, m.Close())
}

// --- MemfdCache ----------------------------------------------------------

func TestMemfdCache_FullRange(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	size := pageSize * 30

	fd, expected := newTestMemfd(t, size)

	ranges := []Range{{Start: 0, Size: size}}

	cache, err := NewCacheFromMemfd(t.Context(), pageSize, t.TempDir()+"/cache", NewFromFd(fd, int(size)), ranges)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	got := make([]byte, size)
	n, err := cache.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, int(size), n)
	require.Equal(t, expected, got)
}

func TestMemfdCache_MultipleRanges(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	numPages := int64(6)
	size := pageSize * numPages

	fd, expected := newTestMemfd(t, size)

	// Pages 0, 2, 5 — non-contiguous, packs the cache in iteration order.
	ranges := []Range{
		{Start: 0, Size: pageSize},
		{Start: pageSize * 2, Size: pageSize},
		{Start: pageSize * 5, Size: pageSize},
	}

	cache, err := NewCacheFromMemfd(t.Context(), pageSize, t.TempDir()+"/cache", NewFromFd(fd, int(size)), ranges)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	cases := []struct {
		cacheOffset int64
		srcOffset   int64
	}{
		{0, 0},
		{pageSize, pageSize * 2},
		{pageSize * 2, pageSize * 5},
	}
	for _, tc := range cases {
		got := make([]byte, pageSize)
		n, err := cache.ReadAt(got, tc.cacheOffset)
		require.NoError(t, err)
		require.Equal(t, int(pageSize), n)
		require.Equal(t, expected[tc.srcOffset:tc.srcOffset+pageSize], got,
			"page at cache offset %d", tc.cacheOffset)
	}
}

// Regression: writeToDisk used to index src[srcOff:...] with srcOff in
// guest-absolute space, which panicked the first time a Range.Start was > 0.
// This test would have caught it.
func TestMemfdCache_NonZeroRangeStart(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	numPages := int64(8)
	size := pageSize * numPages

	fd, expected := newTestMemfd(t, size)

	// Skip the first three pages entirely; the only Range starts at
	// pageSize*3 — this is the case that used to panic.
	ranges := []Range{{Start: pageSize * 3, Size: pageSize * 2}}

	cache, err := NewCacheFromMemfd(t.Context(), pageSize, t.TempDir()+"/cache", NewFromFd(fd, int(size)), ranges)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	got := make([]byte, pageSize*2)
	n, err := cache.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, int(pageSize*2), n)
	require.Equal(t, expected[pageSize*3:pageSize*5], got)
}

// Range.Size > blockSize forces writeToDisk's inner chunked copy loop to
// iterate more than once. Verifies correctness across chunk boundaries.
func TestMemfdCache_RangeLargerThanBlockSize(t *testing.T) {
	t.Parallel()

	blockSize := int64(header.PageSize) // 4 KiB
	rangeSize := blockSize * 5          // 5 chunks per Range
	size := rangeSize + blockSize       // memfd a bit larger than the Range
	fd, expected := newTestMemfd(t, size)

	ranges := []Range{{Start: 0, Size: rangeSize}}

	cache, err := NewCacheFromMemfd(t.Context(), blockSize, t.TempDir()+"/cache", NewFromFd(fd, int(size)), ranges)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	got := make([]byte, rangeSize)
	n, err := cache.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, int(rangeSize), n)
	require.Equal(t, expected[:rangeSize], got)
}

// Exercises the BitsetRanges-derived merged-range path: pages 1,2 merge
// into one Range, page 6 is separate. Mirrors the runtime exportMemory flow.
func TestMemfdCache_DirtyBitmap(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	numPages := int64(8)
	size := pageSize * numPages

	fd, expected := newTestMemfd(t, size)

	dirty := roaring.New()
	dirty.Add(1)
	dirty.Add(2)
	dirty.Add(6)

	var ranges []Range
	for r := range BitsetRanges(dirty, pageSize) {
		ranges = append(ranges, r)
	}

	cache, err := NewCacheFromMemfd(t.Context(), pageSize, t.TempDir()+"/cache", NewFromFd(fd, int(size)), ranges)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	cases := []struct {
		cacheOffset, srcOffset, length int64
	}{
		{0, pageSize, pageSize * 2}, // merged 1–2
		{pageSize * 2, pageSize * 6, pageSize},
	}
	for _, tc := range cases {
		got := make([]byte, tc.length)
		n, err := cache.ReadAt(got, tc.cacheOffset)
		require.NoError(t, err)
		require.Equal(t, int(tc.length), n)
		require.Equal(t, expected[tc.srcOffset:tc.srcOffset+tc.length], got,
			"chunk at cache offset %d", tc.cacheOffset)
	}
}

func TestMemfdCache_EmptyRanges(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	fd, _ := newTestMemfd(t, pageSize*4)

	cache, err := NewCacheFromMemfd(t.Context(), pageSize, t.TempDir()+"/cache", NewFromFd(fd, int(pageSize*4)), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	sz, err := cache.Size()
	require.NoError(t, err)
	require.EqualValues(t, 0, sz)
}

func TestMemfdCache_ContextCancellation(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	size := pageSize * 16
	fd, _ := newTestMemfd(t, size)

	ranges := []Range{{Start: 0, Size: size}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NewCacheFromMemfd(ctx, pageSize, t.TempDir()+"/cache", NewFromFd(fd, int(size)), ranges)
	require.ErrorIs(t, err, context.Canceled)
}

// On the happy path, NewCacheFromMemfd closes the memfd internally and nils
// the field, so subsequent MemfdCache.Close must still cleanly close the
// underlying *Cache without trying to re-close the memfd. The cache file is
// then removed by Cache.Close.
func TestMemfdCache_CloseAfterSuccessfulPopulationRemovesCacheFile(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	size := pageSize * 4
	fd, _ := newTestMemfd(t, size)

	cachePath := t.TempDir() + "/cache"
	cache, err := NewCacheFromMemfd(t.Context(), pageSize, cachePath, NewFromFd(fd, int(size)), []Range{{Start: 0, Size: size}})
	require.NoError(t, err)

	// File exists while the cache is alive.
	_, err = os.Stat(cachePath)
	require.NoError(t, err)

	require.NoError(t, cache.Close())

	// Cache.Close removes the backing file.
	_, err = os.Stat(cachePath)
	require.ErrorIs(t, err, os.ErrNotExist, "expected cache file to be removed, got: %v", err)
}

// --- NewCacheFromMemfdDeduped --------------------------------------------

// newTestMemfdWith creates a memfd populated with the given bytes. Unlike
// newTestMemfd it lets the caller dictate exact contents — needed for dedup
// tests that arrange specific match/differ patterns against a base.
func newTestMemfdWith(t *testing.T, data []byte) int {
	t.Helper()

	fd, err := unix.MemfdCreate("test", 0)
	require.NoError(t, err)
	require.NoError(t, unix.Ftruncate(fd, int64(len(data))))

	if len(data) > 0 {
		_, err = syscall.Pwrite(fd, data, 0)
		require.NoError(t, err)
	}

	return fd
}

// fakeOriginalDevice satisfies ReadonlyDevice over a fixed byte buffer.
// Only ReadAt is exercised by NewCacheFromMemfdDeduped; the rest are stubs.
type fakeOriginalDevice struct {
	data []byte
}

func (f *fakeOriginalDevice) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

func (f *fakeOriginalDevice) Size(context.Context) (int64, error)                 { return int64(len(f.data)), nil }
func (f *fakeOriginalDevice) Close() error                                        { return nil }
func (f *fakeOriginalDevice) Slice(context.Context, int64, int64) ([]byte, error) { return nil, nil }
func (f *fakeOriginalDevice) BlockSize() int64                                    { return int64(header.PageSize) }
func (f *fakeOriginalDevice) Header() *header.Header                              { return nil }
func (f *fakeOriginalDevice) SwapHeader(*header.Header)                           {}

// erroringOriginalDevice returns sentinel from every ReadAt. Used to verify
// that NewCacheFromMemfdDeduped propagates base-read failures.
type erroringOriginalDevice struct {
	fakeOriginalDevice

	err error
}

func (e *erroringOriginalDevice) ReadAt(context.Context, []byte, int64) (int, error) {
	return 0, e.err
}

func TestNewCacheFromMemfdDeduped_AllPagesMatch(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize * 2 // 8 pages

	data := make([]byte, size)
	_, err := rand.Read(data)
	require.NoError(t, err)

	fd := newTestMemfdWith(t, data) // memfd content == base content
	memfd := NewFromFd(fd, int(size))

	ranges := []Range{{Start: 0, Size: size}}

	cache, meta, err := NewCacheFromMemfdDeduped(
		t.Context(),
		&fakeOriginalDevice{data: data},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		ranges,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	sz, err := cache.Size()
	require.NoError(t, err)
	require.EqualValues(t, 0, sz, "no pages differ — dedup cache must be empty")

	require.EqualValues(t, 0, meta.Dirty.GetCardinality())
	require.EqualValues(t, 0, meta.Empty.GetCardinality())
	require.EqualValues(t, header.PageSize, meta.BlockSize)
}

func TestNewCacheFromMemfdDeduped_AllPagesDiffer(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize * 2 // 8 pages

	srcData := make([]byte, size)
	_, err := rand.Read(srcData)
	require.NoError(t, err)
	origData := make([]byte, size) // all zeros — every page differs

	fd := newTestMemfdWith(t, srcData)
	memfd := NewFromFd(fd, int(size))

	ranges := []Range{{Start: 0, Size: size}}

	cache, meta, err := NewCacheFromMemfdDeduped(
		t.Context(),
		&fakeOriginalDevice{data: origData},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		ranges,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	numPages := size / pageSize
	require.EqualValues(t, numPages, meta.Dirty.GetCardinality())
	require.EqualValues(t, header.PageSize, meta.BlockSize)

	// Every page is packed at index i × pageSize and equals srcData's page i.
	for i := range numPages {
		got := make([]byte, pageSize)
		n, err := cache.ReadAt(got, i*pageSize)
		require.NoError(t, err)
		require.Equal(t, int(pageSize), n)
		require.Equal(t, srcData[i*pageSize:(i+1)*pageSize], got, "page %d", i)
	}
}

func TestNewCacheFromMemfdDeduped_MixedPages(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize * 3 // 12 pages across 3 blocks

	origData := make([]byte, size)
	_, err := rand.Read(origData)
	require.NoError(t, err)

	srcData := make([]byte, size)
	copy(srcData, origData)

	// Differ pages 2 and 7.
	differingPage2 := bytes.Repeat([]byte{0xAA}, int(pageSize))
	differingPage7 := bytes.Repeat([]byte{0xBB}, int(pageSize))
	copy(srcData[2*pageSize:], differingPage2)
	copy(srcData[7*pageSize:], differingPage7)

	fd := newTestMemfdWith(t, srcData)
	memfd := NewFromFd(fd, int(size))

	ranges := []Range{{Start: 0, Size: size}}

	cache, meta, err := NewCacheFromMemfdDeduped(
		t.Context(),
		&fakeOriginalDevice{data: origData},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		ranges,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	require.EqualValues(t, 2, meta.Dirty.GetCardinality())
	require.True(t, meta.Dirty.Contains(2))
	require.True(t, meta.Dirty.Contains(7))

	got := make([]byte, pageSize)
	_, err = cache.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, differingPage2, got, "first packed page is the differing page at idx 2")

	_, err = cache.ReadAt(got, pageSize)
	require.NoError(t, err)
	require.Equal(t, differingPage7, got, "second packed page is the differing page at idx 7")
}

// Regression: dedupRange used to index src[srcOff:...] with srcOff in
// guest-absolute space, which panicked on any Range.Start > 0.
func TestNewCacheFromMemfdDeduped_NonZeroRangeStart(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize * 3 // 12 pages

	origData := make([]byte, size)
	_, err := rand.Read(origData)
	require.NoError(t, err)

	srcData := make([]byte, size)
	copy(srcData, origData)

	// Differ one page well past the start of the memfd.
	differing := bytes.Repeat([]byte{0xCC}, int(pageSize))
	copy(srcData[9*pageSize:], differing)

	fd := newTestMemfdWith(t, srcData)
	memfd := NewFromFd(fd, int(size))

	// Range covers only the third block; Start is non-zero.
	ranges := []Range{{Start: blockSize * 2, Size: blockSize}}

	cache, meta, err := NewCacheFromMemfdDeduped(
		t.Context(),
		&fakeOriginalDevice{data: origData},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		ranges,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	require.EqualValues(t, 1, meta.Dirty.GetCardinality())
	require.True(t, meta.Dirty.Contains(9))

	got := make([]byte, pageSize)
	_, err = cache.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, differing, got)
}

// Two non-contiguous Ranges → verifies cacheOff advances correctly across
// independent dedupRange calls, and packed output preserves iteration order.
func TestNewCacheFromMemfdDeduped_MultipleRanges(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize * 4 // 16 pages

	origData := make([]byte, size)
	_, err := rand.Read(origData)
	require.NoError(t, err)

	srcData := make([]byte, size)
	copy(srcData, origData)

	// Differ page 1 (inside first range) and page 13 (inside last range).
	p1 := bytes.Repeat([]byte{0xD1}, int(pageSize))
	p13 := bytes.Repeat([]byte{0xD2}, int(pageSize))
	copy(srcData[1*pageSize:], p1)
	copy(srcData[13*pageSize:], p13)

	fd := newTestMemfdWith(t, srcData)
	memfd := NewFromFd(fd, int(size))

	// Two non-contiguous ranges: blocks 0 and 3.
	ranges := []Range{
		{Start: 0, Size: blockSize},
		{Start: blockSize * 3, Size: blockSize},
	}

	cache, meta, err := NewCacheFromMemfdDeduped(
		t.Context(),
		&fakeOriginalDevice{data: origData},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		ranges,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	require.EqualValues(t, 2, meta.Dirty.GetCardinality())
	require.True(t, meta.Dirty.Contains(1))
	require.True(t, meta.Dirty.Contains(13))

	got := make([]byte, pageSize)
	_, err = cache.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, p1, got)

	_, err = cache.ReadAt(got, pageSize)
	require.NoError(t, err)
	require.Equal(t, p13, got)
}

func TestNewCacheFromMemfdDeduped_EmptyRanges(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize

	data := make([]byte, size)
	fd := newTestMemfdWith(t, data)
	memfd := NewFromFd(fd, int(size))

	cache, meta, err := NewCacheFromMemfdDeduped(
		t.Context(),
		&fakeOriginalDevice{data: data},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	sz, err := cache.Size()
	require.NoError(t, err)
	require.EqualValues(t, 0, sz)
	require.EqualValues(t, 0, meta.Dirty.GetCardinality())
	require.EqualValues(t, 0, meta.Empty.GetCardinality())
}

func TestNewCacheFromMemfdDeduped_ContextCancellation(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize * 4

	data := make([]byte, size)
	_, err := rand.Read(data)
	require.NoError(t, err)

	fd := newTestMemfdWith(t, data)
	memfd := NewFromFd(fd, int(size))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err = NewCacheFromMemfdDeduped(
		ctx,
		&fakeOriginalDevice{data: data},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		[]Range{{Start: 0, Size: size}},
	)
	require.ErrorIs(t, err, context.Canceled)
}

func TestNewCacheFromMemfdDeduped_OriginalMemfileReadError(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize

	data := make([]byte, size)
	_, err := rand.Read(data)
	require.NoError(t, err)

	fd := newTestMemfdWith(t, data)
	memfd := NewFromFd(fd, int(size))

	sentinel := errors.New("base read failed")
	_, _, err = NewCacheFromMemfdDeduped(
		t.Context(),
		&erroringOriginalDevice{
			fakeOriginalDevice: fakeOriginalDevice{data: data},
			err:                sentinel,
		},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		[]Range{{Start: 0, Size: size}},
	)
	require.ErrorIs(t, err, sentinel)
}

// On the happy path NewCacheFromMemfdDeduped closes the memfd internally.
// MemfdCache.Close must still cleanly close the inner *Cache (which removes
// the on-disk file).
func TestNewCacheFromMemfdDeduped_CloseRemovesCacheFile(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize

	origData := make([]byte, size)
	srcData := make([]byte, size)
	_, err := rand.Read(srcData)
	require.NoError(t, err)

	fd := newTestMemfdWith(t, srcData)
	memfd := NewFromFd(fd, int(size))

	cachePath := t.TempDir() + "/dedup"
	cache, _, err := NewCacheFromMemfdDeduped(
		t.Context(),
		&fakeOriginalDevice{data: origData},
		blockSize,
		cachePath,
		memfd,
		[]Range{{Start: 0, Size: size}},
	)
	require.NoError(t, err)

	_, err = os.Stat(cachePath)
	require.NoError(t, err, "cache file should exist while MemfdCache is alive")

	require.NoError(t, cache.Close())

	_, err = os.Stat(cachePath)
	require.ErrorIs(t, err, os.ErrNotExist, "cache file should be removed after Close")
}

// Pages that match the base and happen to be all-zero must be recorded in
// Empty (so the merged header maps them to uuid.Nil → zero-fill at read),
// rather than relying on a fall-through to the parent's diff — which for
// the synthetic Empty template has no real backing file and would error.
//
// Non-zero pages that match the base must NOT land in Empty (those rely on
// the merged mapping keeping the parent's mapping pointing at the real
// parent diff).
func TestNewCacheFromMemfdDeduped_ZeroMatchingPagesGoIntoEmpty(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	blockSize := 4 * pageSize
	size := blockSize * 2 // 8 pages

	// Base: pages 0..3 zero, pages 4..7 random non-zero.
	origData := make([]byte, size)
	_, err := rand.Read(origData[4*pageSize:])
	require.NoError(t, err)

	// Memfd matches base exactly — no Dirty pages, but the first half
	// matches a *zero* base and the second half matches a *non-zero* base.
	srcData := make([]byte, size)
	copy(srcData, origData)

	fd := newTestMemfdWith(t, srcData)
	memfd := NewFromFd(fd, int(size))

	cache, meta, err := NewCacheFromMemfdDeduped(
		t.Context(),
		&fakeOriginalDevice{data: origData},
		blockSize,
		t.TempDir()+"/dedup",
		memfd,
		[]Range{{Start: 0, Size: size}},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	require.EqualValues(t, 0, meta.Dirty.GetCardinality(), "no pages differ from base")

	// Only the zero pages (0..3) should be in Empty.
	require.EqualValues(t, 4, meta.Empty.GetCardinality())
	for i := uint32(0); i < 4; i++ {
		require.True(t, meta.Empty.Contains(i), "zero-matching page %d should be in Empty", i)
	}
	for i := uint32(4); i < 8; i++ {
		require.False(t, meta.Empty.Contains(i), "non-zero-matching page %d should not be in Empty", i)
	}
}
