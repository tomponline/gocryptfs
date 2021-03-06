package fusefrontend

// FUSE operations on file handles

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"

	"github.com/rfjakob/gocryptfs/internal/contentenc"
	"github.com/rfjakob/gocryptfs/internal/toggledlog"
)

// File - based on loopbackFile in go-fuse/fuse/nodefs/files.go
type file struct {
	fd *os.File
	// Has Release() already been called on this file? This also means that the
	// wlock entry has been freed, so let's not crash trying to access it.
	// Due to concurrency, Release can overtake other operations. These will
	// return EBADF in that case.
	released bool
	// fdLock prevents the fd to be closed while we are in the middle of
	// an operation.
	// Every FUSE entrypoint should RLock(). The only user of Lock() is
	// Release(), which closes the fd and sets "released" to true.
	fdLock sync.RWMutex

	// Was the file opened O_WRONLY?
	writeOnly bool

	// Content encryption helper
	contentEnc *contentenc.ContentEnc

	// Inode number
	ino uint64

	// File header
	header *contentenc.FileHeader
}

func NewFile(fd *os.File, writeOnly bool, contentEnc *contentenc.ContentEnc) (nodefs.File, fuse.Status) {
	var st syscall.Stat_t
	err := syscall.Fstat(int(fd.Fd()), &st)
	if err != nil {
		toggledlog.Warn.Printf("NewFile: Fstat on fd %d failed: %v\n", fd.Fd(), err)
		return nil, fuse.ToStatus(err)
	}
	wlock.register(st.Ino)

	return &file{
		fd:         fd,
		writeOnly:  writeOnly,
		contentEnc: contentEnc,
		ino:        st.Ino,
	}, fuse.OK
}

// intFd - return the backing file descriptor as an integer. Used for debug
// messages.
func (f *file) intFd() int {
	return int(f.fd.Fd())
}

func (f *file) InnerFile() nodefs.File {
	return nil
}

func (f *file) SetInode(n *nodefs.Inode) {
}

// readHeader - load the file header from disk
//
// Returns io.EOF if the file is empty
func (f *file) readHeader() error {
	buf := make([]byte, contentenc.HEADER_LEN)
	_, err := f.fd.ReadAt(buf, 0)
	if err != nil {
		return err
	}
	h, err := contentenc.ParseHeader(buf)
	if err != nil {
		return err
	}
	f.header = h

	return nil
}

// createHeader - create a new random header and write it to disk
func (f *file) createHeader() error {
	h := contentenc.RandomHeader()
	buf := h.Pack()

	// Prevent partially written (=corrupt) header by preallocating the space beforehand
	err := prealloc(int(f.fd.Fd()), 0, contentenc.HEADER_LEN)
	if err != nil {
		toggledlog.Warn.Printf("ino%d: createHeader: prealloc failed: %s\n", f.ino, err.Error())
		return err
	}

	// Actually write header
	_, err = f.fd.WriteAt(buf, 0)
	if err != nil {
		return err
	}
	f.header = h

	return nil
}

func (f *file) String() string {
	return fmt.Sprintf("cryptFile(%s)", f.fd.Name())
}

// doRead - returns "length" plaintext bytes from plaintext offset "off".
// Arguments "length" and "off" do not have to be block-aligned.
//
// doRead reads the corresponding ciphertext blocks from disk, decrypts them and
// returns the requested part of the plaintext.
//
// Called by Read() for normal reading,
// by Write() and Truncate() for Read-Modify-Write
func (f *file) doRead(off uint64, length uint64) ([]byte, fuse.Status) {

	// Read file header
	if f.header == nil {
		err := f.readHeader()
		if err == io.EOF {
			return nil, fuse.OK
		}
		if err != nil {
			return nil, fuse.ToStatus(err)
		}
	}

	// Read the backing ciphertext in one go
	blocks := f.contentEnc.ExplodePlainRange(off, length)
	alignedOffset, alignedLength := blocks[0].JointCiphertextRange(blocks)
	skip := blocks[0].Skip
	toggledlog.Debug.Printf("JointCiphertextRange(%d, %d) -> %d, %d, %d", off, length, alignedOffset, alignedLength, skip)
	ciphertext := make([]byte, int(alignedLength))
	n, err := f.fd.ReadAt(ciphertext, int64(alignedOffset))
	if err != nil && err != io.EOF {
		toggledlog.Warn.Printf("read: ReadAt: %s", err.Error())
		return nil, fuse.ToStatus(err)
	}
	// Truncate ciphertext buffer down to actually read bytes
	ciphertext = ciphertext[0:n]

	firstBlockNo := blocks[0].BlockNo
	toggledlog.Debug.Printf("ReadAt offset=%d bytes (%d blocks), want=%d, got=%d", alignedOffset, firstBlockNo, alignedLength, n)

	// Decrypt it
	plaintext, err := f.contentEnc.DecryptBlocks(ciphertext, firstBlockNo, f.header.Id)
	if err != nil {
		curruptBlockNo := firstBlockNo + f.contentEnc.PlainOffToBlockNo(uint64(len(plaintext)))
		cipherOff := f.contentEnc.BlockNoToCipherOff(curruptBlockNo)
		plainOff := f.contentEnc.BlockNoToPlainOff(curruptBlockNo)
		toggledlog.Warn.Printf("ino%d: doRead: corrupt block #%d (plainOff=%d, cipherOff=%d)",
			f.ino, curruptBlockNo, plainOff, cipherOff)
		return nil, fuse.EIO
	}

	// Crop down to the relevant part
	var out []byte
	lenHave := len(plaintext)
	lenWant := int(skip + length)
	if lenHave > lenWant {
		out = plaintext[skip:lenWant]
	} else if lenHave > int(skip) {
		out = plaintext[skip:lenHave]
	}
	// else: out stays empty, file was smaller than the requested offset

	return out, fuse.OK
}

// Read - FUSE call
func (f *file) Read(buf []byte, off int64) (resultData fuse.ReadResult, code fuse.Status) {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()

	toggledlog.Debug.Printf("ino%d: FUSE Read: offset=%d length=%d", f.ino, len(buf), off)

	if f.writeOnly {
		toggledlog.Warn.Printf("ino%d: Tried to read from write-only file", f.ino)
		return nil, fuse.EBADF
	}

	out, status := f.doRead(uint64(off), uint64(len(buf)))

	if status == fuse.EIO {
		toggledlog.Warn.Printf("ino%d: Read failed with EIO, offset=%d, length=%d", f.ino, len(buf), off)
	}
	if status != fuse.OK {
		return nil, status
	}

	toggledlog.Debug.Printf("ino%d: Read: status %v, returning %d bytes", f.ino, status, len(out))
	return fuse.ReadResultData(out), status
}

const FALLOC_FL_KEEP_SIZE = 0x01

// doWrite - encrypt "data" and write it to plaintext offset "off"
//
// Arguments do not have to be block-aligned, read-modify-write is
// performed internally as neccessary
//
// Called by Write() for normal writing,
// and by Truncate() to rewrite the last file block.
func (f *file) doWrite(data []byte, off int64) (uint32, fuse.Status) {

	// Read header from disk, create a new one if the file is empty
	if f.header == nil {
		err := f.readHeader()
		if err == io.EOF {
			err = f.createHeader()

		}
		if err != nil {
			return 0, fuse.ToStatus(err)
		}
	}

	var written uint32
	status := fuse.OK
	dataBuf := bytes.NewBuffer(data)
	blocks := f.contentEnc.ExplodePlainRange(uint64(off), uint64(len(data)))
	for _, b := range blocks {

		blockData := dataBuf.Next(int(b.Length))

		// Incomplete block -> Read-Modify-Write
		if b.IsPartial() {
			// Read
			o, _ := b.PlaintextRange()
			var oldData []byte
			oldData, status = f.doRead(o, f.contentEnc.PlainBS())
			if status != fuse.OK {
				toggledlog.Warn.Printf("ino%d fh%d: RMW read failed: %s", f.ino, f.intFd(), status.String())
				return written, status
			}
			// Modify
			blockData = f.contentEnc.MergeBlocks(oldData, blockData, int(b.Skip))
			toggledlog.Debug.Printf("len(oldData)=%d len(blockData)=%d", len(oldData), len(blockData))
		}

		// Encrypt
		blockOffset, blockLen := b.CiphertextRange()
		blockData = f.contentEnc.EncryptBlock(blockData, b.BlockNo, f.header.Id)
		toggledlog.Debug.Printf("ino%d: Writing %d bytes to block #%d",
			f.ino, uint64(len(blockData))-f.contentEnc.BlockOverhead(), b.BlockNo)

		// Prevent partially written (=corrupt) blocks by preallocating the space beforehand
		err := prealloc(int(f.fd.Fd()), int64(blockOffset), int64(blockLen))
		if err != nil {
			toggledlog.Warn.Printf("ino%d fh%d: doWrite: prealloc failed: %s", f.ino, f.intFd(), err.Error())
			status = fuse.ToStatus(err)
			break
		}

		// Write
		_, err = f.fd.WriteAt(blockData, int64(blockOffset))

		if err != nil {
			toggledlog.Warn.Printf("doWrite: Write failed: %s", err.Error())
			status = fuse.ToStatus(err)
			break
		}
		written += uint32(b.Length)
	}
	return written, status
}

// Write - FUSE call
func (f *file) Write(data []byte, off int64) (uint32, fuse.Status) {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()
	if f.released {
		// The file descriptor has been closed concurrently, which also means
		// the wlock has been freed. Exit here so we don't crash trying to access
		// it.
		toggledlog.Warn.Printf("ino%d fh%d: Write on released file", f.ino, f.intFd())
		return 0, fuse.EBADF
	}
	wlock.lock(f.ino)
	defer wlock.unlock(f.ino)

	toggledlog.Debug.Printf("ino%d: FUSE Write: offset=%d length=%d", f.ino, off, len(data))

	fi, err := f.fd.Stat()
	if err != nil {
		toggledlog.Warn.Printf("Write: Fstat failed: %v", err)
		return 0, fuse.ToStatus(err)
	}
	plainSize := f.contentEnc.CipherSizeToPlainSize(uint64(fi.Size()))
	if f.createsHole(plainSize, off) {
		status := f.zeroPad(plainSize)
		if status != fuse.OK {
			toggledlog.Warn.Printf("zeroPad returned error %v", status)
			return 0, status
		}
	}
	return f.doWrite(data, off)
}

// Release - FUSE call, close file
func (f *file) Release() {
	f.fdLock.Lock()
	if f.released {
		log.Panicf("ino%d fh%d: double release", f.ino, f.intFd())
	}
	f.fd.Close()
	f.released = true
	f.fdLock.Unlock()

	wlock.unregister(f.ino)
}

// Flush - FUSE call
func (f *file) Flush() fuse.Status {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()

	// Since Flush() may be called for each dup'd fd, we don't
	// want to really close the file, we just want to flush. This
	// is achieved by closing a dup'd fd.
	newFd, err := syscall.Dup(int(f.fd.Fd()))

	if err != nil {
		return fuse.ToStatus(err)
	}
	err = syscall.Close(newFd)
	return fuse.ToStatus(err)
}

func (f *file) Fsync(flags int) (code fuse.Status) {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()

	return fuse.ToStatus(syscall.Fsync(int(f.fd.Fd())))
}

// Truncate - FUSE call
func (f *file) Truncate(newSize uint64) fuse.Status {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()
	if f.released {
		// The file descriptor has been closed concurrently.
		toggledlog.Warn.Printf("ino%d fh%d: Truncate on released file", f.ino, f.intFd())
		return fuse.EBADF
	}
	wlock.lock(f.ino)
	defer wlock.unlock(f.ino)

	// Common case first: Truncate to zero
	if newSize == 0 {
		err := syscall.Ftruncate(int(f.fd.Fd()), 0)
		if err != nil {
			toggledlog.Warn.Printf("ino%d fh%d: Ftruncate(fd, 0) returned error: %v", f.ino, f.intFd(), err)
			return fuse.ToStatus(err)
		}
		// Truncate to zero kills the file header
		f.header = nil
		return fuse.OK
	}

	// We need the old file size to determine if we are growing or shrinking
	// the file
	fi, err := f.fd.Stat()
	if err != nil {
		toggledlog.Warn.Printf("ino%d fh%d: Truncate: Fstat failed: %v", f.ino, f.intFd(), err)
		return fuse.ToStatus(err)
	}
	oldSize := f.contentEnc.CipherSizeToPlainSize(uint64(fi.Size()))
	{
		oldB := float32(oldSize) / float32(f.contentEnc.PlainBS())
		newB := float32(newSize) / float32(f.contentEnc.PlainBS())
		toggledlog.Debug.Printf("ino%d: FUSE Truncate from %.2f to %.2f blocks (%d to %d bytes)", f.ino, oldB, newB, oldSize, newSize)
	}

	// File size stays the same - nothing to do
	if newSize == oldSize {
		return fuse.OK
	}

	// File grows
	if newSize > oldSize {

		// File was empty, create new header
		if oldSize == 0 {
			err := f.createHeader()
			if err != nil {
				return fuse.ToStatus(err)
			}
		}

		blocks := f.contentEnc.ExplodePlainRange(oldSize, newSize-oldSize)
		for _, b := range blocks {
			// First and last block may be partial
			if b.IsPartial() {
				off, _ := b.PlaintextRange()
				off += b.Skip
				_, status := f.doWrite(make([]byte, b.Length), int64(off))
				if status != fuse.OK {
					return status
				}
			} else {
				off, length := b.CiphertextRange()
				err := syscall.Ftruncate(int(f.fd.Fd()), int64(off+length))
				if err != nil {
					toggledlog.Warn.Printf("grow Ftruncate returned error: %v", err)
					return fuse.ToStatus(err)
				}
			}
		}
		return fuse.OK
	} else {
		// File shrinks
		blockNo := f.contentEnc.PlainOffToBlockNo(newSize)
		cipherOff := f.contentEnc.BlockNoToCipherOff(blockNo)
		plainOff := f.contentEnc.BlockNoToPlainOff(blockNo)
		lastBlockLen := newSize - plainOff
		var data []byte
		if lastBlockLen > 0 {
			var status fuse.Status
			data, status = f.doRead(plainOff, lastBlockLen)
			if status != fuse.OK {
				toggledlog.Warn.Printf("shrink doRead returned error: %v", err)
				return status
			}
		}
		// Truncate down to last complete block
		err = syscall.Ftruncate(int(f.fd.Fd()), int64(cipherOff))
		if err != nil {
			toggledlog.Warn.Printf("shrink Ftruncate returned error: %v", err)
			return fuse.ToStatus(err)
		}
		// Append partial block
		if lastBlockLen > 0 {
			_, status := f.doWrite(data, int64(plainOff))
			return status
		}
		return fuse.OK
	}
}

func (f *file) Chmod(mode uint32) fuse.Status {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()

	return fuse.ToStatus(f.fd.Chmod(os.FileMode(mode)))
}

func (f *file) Chown(uid uint32, gid uint32) fuse.Status {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()

	return fuse.ToStatus(f.fd.Chown(int(uid), int(gid)))
}

func (f *file) GetAttr(a *fuse.Attr) fuse.Status {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()

	toggledlog.Debug.Printf("file.GetAttr()")
	st := syscall.Stat_t{}
	err := syscall.Fstat(int(f.fd.Fd()), &st)
	if err != nil {
		return fuse.ToStatus(err)
	}
	a.FromStat(&st)
	a.Size = f.contentEnc.CipherSizeToPlainSize(a.Size)

	return fuse.OK
}

// Only warn once
var allocateWarnOnce sync.Once

// Allocate - FUSE call, fallocate(2)
func (f *file) Allocate(off uint64, sz uint64, mode uint32) fuse.Status {
	allocateWarnOnce.Do(func() {
		toggledlog.Warn.Printf("fallocate(2) is not supported, returning ENOSYS - see https://github.com/rfjakob/gocryptfs/issues/1")
	})
	return fuse.ENOSYS
}

const _UTIME_OMIT = ((1 << 30) - 2)

func (f *file) Utimens(a *time.Time, m *time.Time) fuse.Status {
	f.fdLock.RLock()
	defer f.fdLock.RUnlock()

	ts := make([]syscall.Timespec, 2)

	if a == nil {
		ts[0].Nsec = _UTIME_OMIT
	} else {
		ts[0].Sec = a.Unix()
	}

	if m == nil {
		ts[1].Nsec = _UTIME_OMIT
	} else {
		ts[1].Sec = m.Unix()
	}

	fn := fmt.Sprintf("/proc/self/fd/%d", f.fd.Fd())
	err := syscall.UtimesNano(fn, ts)
	if err != nil {
		toggledlog.Debug.Printf("UtimesNano on %q failed: %v", fn, err)
	}
	if err == syscall.ENOENT {
		// If /proc/self/fd/X did not exist, the actual error is that the file
		// descriptor was invalid.
		return fuse.EBADF
	}
	return fuse.ToStatus(err)
}
