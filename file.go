package dropbox // nolint: golint

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/dropbox/dropbox-sdk-go-unofficial/dropbox"
	"github.com/dropbox/dropbox-sdk-go-unofficial/dropbox/files"
	"github.com/spf13/afero"
)

// File represents a file structure.
type File struct {
	fs                  *Fs
	name                string
	streamWrite         io.WriteCloser
	streamRead          io.ReadCloser
	streamWriteCloseErr chan error
	streamWriteErr      error
	dirList             chan os.FileInfo
	dirListCursor       string
	dirListDone         bool
	streamReadOffset    int64
	cachedInfo          os.FileInfo
}

const (
	dirListingMaxLimit = 2000
	simulatedFileMode  = 0777
)

func newFile(fs *Fs, name string) *File {
	return &File{
		fs:                  fs,
		name:                name,
		streamWriteCloseErr: make(chan error),
	}
}

// Close closes the File, rendering it unusable for I/O.
// It returns an error, if any.
func (f *File) Close() error {
	// Closing a reading stream
	if f.streamRead != nil {
		// We try to close the Reader
		defer func() {
			f.streamRead = nil
		}()

		return f.streamRead.Close()
	}

	// Closing a writing stream
	if f.streamWrite != nil {
		defer func() {
			f.streamWrite = nil
			f.streamWriteCloseErr = nil
		}()

		// We try to close the Writer
		if err := f.streamWrite.Close(); err != nil {
			return fmt.Errorf("problem writing file: %w", err)
		}
		// And more importantly, we wait for the actual writing performed in go-routine to finish.
		err := <-f.streamWriteCloseErr
		close(f.streamWriteCloseErr)

		return err
	}

	// Or maybe we don't have anything to close
	return nil
}

// Read reads up to len(b) bytes from the File.
// It returns the number of bytes read and an error, if any.
// EOF is signaled by a zero count with err set to io.EOF.
func (f *File) Read(p []byte) (int, error) {
	n, err := f.streamRead.Read(p)

	if err != nil {
		if errors.Is(err, io.EOF) {
			return n, io.EOF
		}

		return 0, fmt.Errorf("couldn't read from stream: %w", err)
	}

	f.streamReadOffset += int64(n)

	return n, nil
}

// ReadAt reads len(p) bytes from the file starting at byte offset off.
// It returns the number of bytes read and the error, if any.
// ReadAt always returns a non-nil error when n < len(b).
// At end of file, that error is io.EOF.
func (f *File) ReadAt(p []byte, off int64) (n int, err error) {
	if _, err := f.Seek(off, io.SeekCurrent); err != nil {
		return 0, err
	}

	return f.Read(p)
}

// Seek sets the offset for the next Read or Write on file to offset, interpreted
// according to whence: 0 means relative to the origin of the file, 1 means
// relative to the current offset, and 2 means relative to the end.
// It returns the new offset and an error, if any.
// The behavior of Seek on a file opened with O_APPEND is not specified.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	// Write seek is not supported
	if f.streamWrite != nil {
		return 0, ErrNotSupported
	}

	// Read seek has its own implementation
	if f.streamRead != nil {
		return f.seekRead(offset, whence)
	}

	// Not having a stream
	return 0, afero.ErrFileClosed
}

// Write writes len(b) bytes to the File.
// It returns the number of bytes written and an error, if any.
// Write returns a non-nil error when n != len(b).
func (f *File) Write(p []byte) (n int, err error) {
	return f.streamWrite.Write(p)
}

// WriteAt writes len(p) bytes to the file starting at byte offset off.
// It returns the number of bytes written and an error, if any.
// WriteAt returns a non-nil error when n != len(p).
func (f *File) WriteAt(p []byte, off int64) (n int, err error) {
	if _, err := f.Seek(off, io.SeekCurrent); err != nil {
		return 0, err
	}

	return f.Write(p)
}

// Name returns the file name.
func (f *File) Name() string {
	return f.name
}

func newFileInfo(meta files.IsMetadata) os.FileInfo {
	return &FileInfo{meta: meta}
}

// FileInfo is dropbox file description.
type FileInfo struct {
	meta files.IsMetadata
}

// Name returns the file name.
func (f FileInfo) Name() string {
	if file, ok := f.meta.(*files.FileMetadata); ok {
		return file.Name
	} else if folder, ok := f.meta.(*files.FolderMetadata); ok {
		return folder.Name
	} else {
		return ""
	}
}

// Size returns the file size.
func (f FileInfo) Size() int64 {
	if file, ok := f.meta.(*files.FileMetadata); ok {
		return int64(file.Size)
	}

	return 0
}

// Mode return the file mode.
func (f FileInfo) Mode() os.FileMode {
	return simulatedFileMode
}

// ModTime returns the modification time.
func (f FileInfo) ModTime() time.Time {
	if file, ok := f.meta.(*files.FileMetadata); ok {
		return file.ClientModified
	}

	return time.Time{}
}

// IsDir returns if it's a directory.
func (f FileInfo) IsDir() bool {
	_, ok := f.meta.(*files.FolderMetadata)

	return ok
}

// Sys returns the underlying structure.
func (f FileInfo) Sys() interface{} {
	return f.meta
}

func (f *File) _readDirAll() ([]os.FileInfo, error) {
	var fis []os.FileInfo
	dirListCursor := ""
	for {
		var resp *files.ListFolderResult
		var err error
		if len(dirListCursor) == 0 {
			req := &files.ListFolderArg{Path: f.name}
			resp, err = f.fs.files.ListFolder(req)
		} else {
			resp, err = f.fs.files.ListFolderContinue(&files.ListFolderContinueArg{Cursor: dirListCursor})
		}
		if err != nil {
			return nil, err
		}
		for _, m := range resp.Entries {
			fis = append(fis, newFileInfo(m))
		}
		dirListCursor = resp.Cursor
		if !resp.HasMore {
			break
		}
	}
	return fis, nil
}

// Actual fetching of files.
func (f *File) _readDir() error {
	var res *files.ListFolderResult
	var err error

	if f.dirListCursor == "" {
		// We're using a channel as a queue
		f.dirList = make(chan os.FileInfo, dirListingMaxLimit)

		req := &files.ListFolderArg{Path: f.name}

		if f.fs.dirListLimit != 0 {
			req.Limit = uint32(f.fs.dirListLimit)
		}

		// We might want to use the limit here...
		res, err = f.fs.files.ListFolder(req)
	} else {
		res, err = f.fs.files.ListFolderContinue(&files.ListFolderContinueArg{Cursor: f.dirListCursor})
	}

	if err != nil {
		return fmt.Errorf("couldn't fetch files list: %w", err)
	}

	f.dirListCursor = res.Cursor
	f.dirListDone = !res.HasMore

	for _, m := range res.Entries {
		f.dirList <- newFileInfo(m)
	}

	return nil
}

// Readdir lists all the files of a directory.
// Unfortunately the dropbox API doesn't allow to limit the number of returned files per call.
// so what we're doing here is to using a channel a temporary buffer.
// If count <= 0, Readdir returns all the FileInfo from the directory in a single slice.
func (f *File) Readdir(count int) ([]os.FileInfo, error) {
	if count <= 0 {
		return f._readDirAll()
	}
	list := make([]os.FileInfo, 0, count)

	for len(list) < count && !f.dirListDone {
		// If we don't have any available, we should request some
		if len(f.dirList) == 0 {
			if err := f._readDir(); err != nil {
				return nil, err
			}
		}

		for len(list) < count && len(f.dirList) > 0 {
			list = append(list, <-f.dirList)
		}
	}

	return list, nil
}

// Readdirnames reads and returns a slice of names from the directory f.
func (f *File) Readdirnames(n int) ([]string, error) {
	fi, err := f.Readdir(n)

	if err != nil {
		return nil, err
	}

	names := make([]string, len(fi))

	for i, f := range fi {
		_, names[i] = path.Split(f.Name())
	}

	return names, nil
}

// Stat fetches the file stat with a cache.
func (f *File) Stat() (os.FileInfo, error) {
	var err error

	if f.cachedInfo == nil {
		f.cachedInfo, err = f.fs.stat(f.name)
	}

	return f.cachedInfo, err
}

// Sync doesn't do anything.
func (f *File) Sync() error {
	return nil
}

// Truncate should truncate a file to a specific size but isn't
// supported by dropbox.
func (f *File) Truncate(size int64) error {
	return ErrNotSupported
}

// WriteString writes a string.
func (f *File) WriteString(s string) (ret int, err error) {
	return f.Write([]byte(s))
}

func (f *File) openWriteStream() error {
	if f.streamWrite != nil {
		return ErrAlreadyOpened
	}

	f.cachedInfo = nil

	reader, writer := io.Pipe()

	f.streamWriteCloseErr = make(chan error)
	f.streamWrite = writer

	go func() {
		req := &files.CommitInfo{
			Path: f.name,
			// Dropbox API has a BUG. TODO: Report it
			//ClientModified: time.Now().UTC(),
			Mode:       &files.WriteMode{Tagged: dropbox.Tagged{Tag: "overwrite"}},
			Autorename: false,
		}
		meta, err := f.fs.files.Upload(req, reader)

		if err != nil {
			f.streamWriteErr = err
			_ = f.streamWrite.Close()
		}

		f.cachedInfo = newFileInfo(meta)
		f.streamWriteCloseErr <- err
	}()

	return nil
}

func (f *File) openReadStream(startAt int64) error {
	var err error

	f.streamReadOffset = startAt

	req := &files.DownloadArg{
		Path:         f.name,
		ExtraHeaders: make(map[string]string),
	}

	if startAt > 0 {
		req.ExtraHeaders["Range"] = fmt.Sprintf("bytes=%d-", startAt)
	}

	_, f.streamRead, err = f.fs.files.Download(req)

	if err != nil {
		return fmt.Errorf("couldn't download file: %w", err)
	}

	return nil
}

func (f *File) seekRead(offset int64, whence int) (int64, error) {
	startByte := int64(0)

	switch whence {
	case io.SeekStart:
		startByte = offset
	case io.SeekCurrent:
		startByte = f.streamReadOffset + offset
	case io.SeekEnd:
		startByte = f.cachedInfo.Size() - offset
	}

	if err := f.streamRead.Close(); err != nil {
		return 0, fmt.Errorf("couldn't close previous stream: %w", err)
	}

	f.streamRead = nil

	if startByte < 0 {
		return startByte, ErrInvalidSeek
	}

	return startByte, f.openReadStream(startByte)
}
