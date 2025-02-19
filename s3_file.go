// Package s3 brings S3 files handling to afero
package s3

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/spf13/afero"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

// File represents a file in S3.
// nolint: maligned
type File struct {
	fs                       *Fs            // Parent file system
	name                     string         // Name of the file
	cachedInfo               os.FileInfo    // File info cached for later used
	streamRead               io.ReadCloser  // streamRead is the underlying stream we are reading from
	streamReadOffset         int64          // streamReadOffset is the offset of the read-only stream
	streamReadSize           int64          // streamReadSize is the size of the read-only stream
	streamWrite              io.WriteCloser // streamWrite is the underlying stream we are reading to
	streamWriteErr           error          // streamWriteErr is the error that should be returned in case of a write
	streamWriteCloseErr      chan error     // streamWriteCloseErr is the channel containing the underlying write error
	readdirContinuationToken *string        // readdirContinuationToken is used to perform files listing across calls
	readdirNotTruncated      bool           // readdirNotTruncated is set when we shall continue reading
	// I think readdirNotTruncated can be dropped. The continuation token is probably enough.
}

// NewFile initializes an File object.
func NewFile(fs *Fs, name string) *File {
	return &File{
		fs:   fs,
		name: name,
	}
}

// Name returns the filename, i.e. S3 path without the bucket name.
func (f *File) Name() string { return f.name }

// Readdir reads the contents of the directory associated with file and
// returns a slice of up to n FileInfo values, as would be returned
// by ListObjects, in directory order. Subsequent calls on the same file will yield further FileInfos.
//
// If n > 0, Readdir returns at most n FileInfo structures. In this case, if
// Readdir returns an empty slice, it will return a non-nil error
// explaining why. At the end of a directory, the error is io.EOF.
//
// If n <= 0, Readdir returns all the FileInfo from the directory in
// a single slice. In this case, if Readdir succeeds (reads all
// the way to the end of the directory), it returns the slice and a
// nil error. If it encounters an error before the end of the
// directory, Readdir returns the FileInfo read until that point
// and a non-nil error.
func (f *File) Readdir(n int) ([]os.FileInfo, error) {
	if f.readdirNotTruncated {
		return nil, io.EOF
	}
	if n <= 0 {
		return f.ReaddirAll()
	}
	// ListObjects treats leading slashes as part of the directory name
	// It also needs a trailing slash to list contents of a directory.
	name := trimLeadingSlash(f.Name()) // + "/"
	// For the root of the bucket, we need to remove any prefix
	if name != "" {
		name += "/"
	}
	output, err := f.fs.s3API.ListObjectsV2(&s3.ListObjectsV2Input{
		ContinuationToken: f.readdirContinuationToken,
		Bucket:            aws.String(f.fs.bucket),
		Prefix:            aws.String(name),
		Delimiter:         aws.String("/"),
		MaxKeys:           aws.Int64(int64(n)),
	})
	if err != nil {
		return nil, err
	}
	f.readdirContinuationToken = output.NextContinuationToken
	if !(aws.BoolValue(output.IsTruncated)) {
		f.readdirNotTruncated = true
	}
	var fis = make([]os.FileInfo, 0, len(output.CommonPrefixes)+len(output.Contents))
	//goland:noinspection SpellCheckingInspection
	for _, subfolder := range output.CommonPrefixes {
		fis = append(fis, NewFileInfo(path.Base("/"+aws.StringValue(subfolder.Prefix)), true, 0, time.Unix(0, 0)))
	}
	for _, fileObject := range output.Contents {
		if hasTrailingSlash(aws.StringValue(fileObject.Key)) {
			// S3 includes <name>/ in the Contents listing for <name>
			continue
		}
		fis = append(fis, NewFileInfo(
			path.Base("/"+aws.StringValue(fileObject.Key)),
			false,
			aws.Int64Value(fileObject.Size),
			aws.TimeValue(fileObject.LastModified)),
		)
	}
	return fis, nil
}

// ReaddirAll provides list of file cachedInfo.
func (f *File) ReaddirAll() ([]os.FileInfo, error) {
	var fileInfos []os.FileInfo
	for {
		infos, err := f.Readdir(100)
		fileInfos = append(fileInfos, infos...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			} else {
				return nil, err
			}
		}
	}
	return fileInfos, nil
}

// Readdirnames reads and returns a slice of names from the directory f.
//
// If n > 0, Readdirnames returns at most n names. In this case, if
// Readdirnames returns an empty slice, it will return a non-nil error
// explaining why. At the end of a directory, the error is io.EOF.
//
// If n <= 0, Readdirnames returns all the names from the directory in
// a single slice. In this case, if Readdirnames succeeds (reads all
// the way to the end of the directory), it returns the slice and a
// nil error. If it encounters an error before the end of the
// directory, Readdirnames returns the names read until that point and
// a non-nil error.
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

// Stat returns the FileInfo structure describing file.
// If there is an error, it will be of type *PathError.
func (f *File) Stat() (os.FileInfo, error) {
	info, err := f.fs.Stat(f.Name())
	if err == nil {
		f.cachedInfo = info
	}
	return info, err
}

// Sync is a noop.
func (f *File) Sync() error {
	return nil
}

// Truncate changes the size of the file.
// It does not change the I/O offset.
// If there is an error, it will be of type *PathError.
func (f *File) Truncate(int64) error {
	return ErrNotImplemented
}

// WriteString is like Write, but writes the contents of string s rather than
// a slice of bytes.
func (f *File) WriteString(s string) (int, error) {
	return f.Write([]byte(s))
}

// Close closes the File, rendering it unusable for I/O.
// It returns an error, if any.
func (f *File) Close() error {
	// Delete info from cache
	f.fs.cachedInfo.Del(f.name)
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
			return err
		}
		// And more importantly, we wait for the actual writing performed in go-routine to finish.
		// We might have at most 2*5=10MB of data waiting to be flushed before close returns. This
		// might be rather slow.
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
	size := len(p)
	if f.streamReadSize < 1 && size > 0 {
		if _, err := f.seekRead(f.streamReadOffset, int64(size), io.SeekStart); err != nil {
			return 0, err
		}
	}
	n := 0
	for {
		r, err := f.streamRead.Read(p[n:])
		if r > 0 {
			n += r
		}
		if err == nil || err == io.EOF {
			f.streamReadOffset += int64(r)
			f.streamReadSize -= int64(r)
			if f.streamReadSize <= 0 || n >= size {
				break
			}
		} else if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ReadAt reads len(p) bytes from the file starting at byte offset off.
// It returns the number of bytes read and the error, if any.
// ReadAt always returns a non-nil error when n < len(b).
// At end of file, that error is io.EOF.
func (f *File) ReadAt(p []byte, off int64) (n int, err error) {
	if f.streamWrite != nil {
		return 0, ErrNotSupported
	}
	if f.streamRead == nil {
		return 0, afero.ErrFileClosed
	}
	_, err = f.seekRead(off, int64(len(p)), io.SeekStart)
	if err != nil {
		return
	}
	n, err = f.Read(p)
	return
}

func (f *File) ReadAtWithSize(p []byte, off, size int64) (n int, err error) {
	if f.streamWrite != nil {
		return 0, ErrNotSupported
	}
	if f.streamRead == nil {
		return 0, afero.ErrFileClosed
	}
	_, err = f.seekRead(off, size, io.SeekStart)
	if err != nil {
		return
	}
	n, err = f.Read(p)
	return
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
		return f.seekRead(offset, 0, whence)
	}
	// Not having a stream
	return 0, afero.ErrFileClosed
}

func (f *File) SeekWithSize(offset, size int64, whence int) (int64, error) {
	// Write seek is not supported
	if f.streamWrite != nil {
		return 0, ErrNotSupported
	}
	// Read seek has its own implementation
	if f.streamRead != nil {
		return f.seekRead(offset, size, whence)
	}
	// Not having a stream
	return 0, afero.ErrFileClosed
}

func (f *File) seekRead(off, size int64, whence int) (int64, error) {
	offset := int64(0)
	switch whence {
	case io.SeekStart:
		offset = off
	case io.SeekCurrent:
		offset = f.streamReadOffset + off
	case io.SeekEnd:
		offset = f.cachedInfo.Size() - off
	}
	if err := f.streamRead.Close(); err != nil {
		return 0, fmt.Errorf("couldn't close previous stream: %w", err)
	}
	f.streamRead, f.streamReadSize = nil, 0
	if offset < 0 {
		return offset, ErrInvalidSeek
	}
	return offset, f.openReadStream(offset, size)
}

// Write writes len(b) bytes to the File.
// It returns the number of bytes written and an error, if any.
// Write returns a non-nil error when n != len(b).
func (f *File) Write(p []byte) (int, error) {
	n, err := f.streamWrite.Write(p)
	// If we have an error, it's only the "read/write on closed pipe" and we
	// should report the underlying one
	if err != nil {
		return 0, f.streamWriteErr
	}
	return n, err
}

func (f *File) openWriteStream() error {
	if f.streamWrite != nil {
		return ErrAlreadyOpened
	}
	reader, writer := io.Pipe()

	f.streamWriteCloseErr = make(chan error)
	f.streamWrite = writer

	uploader := s3manager.NewUploader(f.fs.session)
	uploader.Concurrency = 1

	go func() {
		input := &s3manager.UploadInput{
			Bucket: aws.String(f.fs.bucket),
			Key:    aws.String(f.name),
			Body:   reader,
		}
		if f.fs.FileProps != nil {
			applyFileWriteProps(input, f.fs.FileProps)
		}
		// If no Content-Type was specified, we'll guess one
		if input.ContentType == nil {
			input.ContentType = aws.String(mime.TypeByExtension(filepath.Ext(f.name)))
		}
		_, err := uploader.Upload(input)
		if err != nil {
			f.streamWriteErr = err
			_ = f.streamWrite.Close()
		}
		f.streamWriteCloseErr <- err
		// close(f.streamWriteCloseErr)
	}()
	return nil
}

func (f *File) openReadStream(offset, size int64) error {
	if f.streamRead != nil {
		return ErrAlreadyOpened
	}
	var (
		streamRange *string = nil
		readSize    int64
	)
	if offset > 0 || size > 0 {
		end, objectSize := offset+size, f.cachedInfo.Size()
		if size <= 0 {
			end = objectSize
		} else if end > objectSize {
			end = objectSize
		}
		streamRange = aws.String(fmt.Sprintf("bytes=%d-%d", offset, end))
		readSize = end - offset
	} else {
		readSize = f.cachedInfo.Size()
	}
	if readSize <= 0 && offset > 0 && size > 0 {
		return io.EOF
	}
	resp, err := f.fs.s3API.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(f.fs.bucket),
		Key:    aws.String(f.name),
		Range:  streamRange,
	})
	if err != nil {
		return err
	}
	f.streamReadOffset = offset
	f.streamReadSize = readSize
	f.streamRead = resp.Body
	return nil
}

// WriteAt writes len(p) bytes to the file starting at byte offset off.
// It returns the number of bytes written and an error, if any.
// WriteAt returns a non-nil error when n != len(p).
func (f *File) WriteAt(p []byte, off int64) (n int, err error) {
	_, err = f.Seek(off, 0)
	if err != nil {
		return
	}
	n, err = f.Write(p)
	return
}
