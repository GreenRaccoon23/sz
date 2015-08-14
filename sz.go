package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/golang/snappy"
)

var (
	buffer          bytes.Buffer
	doSingleArchive bool
	doQuiet         bool
	dstArchive      string
	trgtFiles       []string
)

func init() {
	chkHelp()
	flags()
}

// Check whether user requested help.
func chkHelp() {
	if len(os.Args) < 2 {
		return
	}

	switch os.Args[1] {
	case "-h", "h", "help", "--help", "-H", "H", "HELP", "--HELP", "-help", "--h", "--H":
		help(0)
	}

}

// Print help and exit with a status code.
func help(status int) {
	defer os.Exit(status)
	fmt.Printf(
		//"%s\n\n  %s\n\n  %s\n%s\n\n  %s\n%s\n%s\n%s\n\n  %s\n%s\n%s\n%s\n%s\n",
		"%s\n\n  %s\n\n  %s\n%s\n\n  %s\n%s\n\n  %s\n%s\n%s\n%s\n%s\n",
		"sz",
		"Usage: sz [option ...] [file ...]",
		"Description:",
		"    Compress/uncompress files to/from snappy archives.",
		"Options:",
		//"   -a <name>    Compress all files into a single snappy archive.",
		//"                (default is to compress each file individually)",
		"   -q           Do not show any output",
		"Notes:",
		"    This program automatically determines whether a file should be",
		"      compressed or decompressed.",
		"    This program can also compress directories;",
		"      they are added to a tar archive prior to compression.",
	)
}

// Parse user arguments and modify global variables accordingly.
func flags() {
	// Program requires at least one user argument.
	// Print help and exit with status 1 if none have been received.
	if len(os.Args) < 2 {
		help(1)
	}

	// Parse commandline arguments.
	flag.StringVar(&dstArchive, "a", "", "")
	flag.BoolVar(&doQuiet, "q", false, "")
	flag.Parse()

	// Modify global variables based on commandline arguments.
	trgtFiles = os.Args[1:]
	if !doQuiet && dstArchive == "" {
		return
	}

	if doQuiet {
		bools := []string{"-s", "-q"}
		trgtFiles = filter(trgtFiles, bools...)
	}
	if dstArchive != "" {
		doSingleArchive = true
		trgtFiles = filter(trgtFiles, dstArchive)
	}
	return
}

// Remove elements in a slice (if they exist).
// Only remove EXACT matches.
func filter(slc []string, args ...string) (filtered []string) {
	for _, s := range slc {
		if slcHas(slc, s) {
			continue
		}
		filtered = append(filtered, s)
	}
	return
}

// Check whether a slice contains a string.
// Only return true if an element in the slice EXACTLY matches the string.
// If testing for more than one string,
//   return true if ANY of them match an element in the slice.
func slcHas(slc []string, args ...string) bool {
	for _, s := range slc {
		for _, a := range args {
			if s == a {
				return true
			}
		}
	}
	return false
}

func main() {
	defer os.Exit(0)
	//if doSingleArchive
	for _, f := range trgtFiles {
		err := analyze(f)
		if err == nil || doQuiet {
			continue
		}
		fmt.Println(err)
	}
}

// Pass to fmt.Println().
func print(a ...interface{}) {
	if doQuiet {
		return
	}
	switch len(a) {
	case 0:
		fmt.Println()
	default:
		fmt.Println(a...)
	}
}

// Pass to fmt.Printf().
func printf(format string, a ...interface{}) {
	if doQuiet {
		return
	}
	fmt.Printf(format, a...)
}

// Concatenate strings.
func concat(slc ...string) (concatenated string) {
	defer buffer.Reset()
	for _, s := range slc {
		buffer.WriteString(s)
	}
	concatenated = buffer.String()
	return
}

// Test whether a string matches at least one in a set of strings.
func matchesOr(s string, conditions ...string) bool {
	for _, c := range conditions {
		if s == c {
			return true
		}
	}
	return false
}

// Determine whether a file should be compressed, uncompressed, or
//   added to a tar archive and then compressed.
func analyze(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func(f *os.File) {
		f.Close()
	}(file)

	switch {

	// If the file is a snappy file, uncompress it.
	case isSz(file):
		// Uncompress it.
		uncompressed, err := unsnap(file)
		if err != nil {
			return err
		}

		// If the uncompressed file is a tar archive, untar it.
		if !isTar(uncompressed) {
			break
		}
		// Remember to remove the uncompressed tar archive.
		defer func() {
			if err == nil {
				os.Remove(uncompressed.Name())
			}
		}()
		if err = untar(uncompressed); err != nil {
			return err
		}

	// If the file is a directory, tar it before compressing it.
	// (Simultaneously compressing and tarring the file
	//   results in a much lower compression ratio.)
	case isDir(file):
		// Tar it.
		file, err = tarDir(file)
		if err != nil {
			return err
		}
		// Remove to close and remove the temporary tar archive.
		defer func() {
			file.Close()
			if err == nil {
				os.Remove(file.Name())
			}
		}()
		fallthrough

	// If the file is any other type, compress it.
	default:
		// Compress it.
		sz, err := snap(file)
		if err == nil {
			break
		}

		// If snap() failed, try the safer function snapSafe().
		os.Remove(sz.Name())
		if _, err = snapSafe(file); err != nil {
			return err
		}
	}

	return nil
}

// Check whether a file is a directory.
func isDir(file *os.File) bool {
	fi, err := file.Stat()
	if err != nil {
		return false
	}
	return fi.IsDir()
}

// Check a file's contents for a snappy file signature.
func isSz(file *os.File) bool {
	total := 10
	bytes := make([]byte, total)
	n, _ := file.ReadAt(bytes, 0)
	if n < total {
		return false
	}

	szSig := []byte{255, 6, 0, 0, 115, 78, 97, 80, 112, 89}
	for i, b := range bytes {
		if b != szSig[i] {
			return false
		}
	}
	return true
}

// Check a file's contents for a tar file signature.
func isTar(file *os.File) bool {
	bytes := make([]byte, 5)
	n, _ := file.ReadAt(bytes, 257)
	if n < 5 {
		return false
	}

	tarSig := []byte{117, 115, 116, 97, 114}
	for i, b := range bytes {
		if b != tarSig[i] {
			return false
		}
	}
	return true
}

// Credits to jimt from here:
// https://stackoverflow.com/questions/22421375/how-to-print-the-bytes-while-the-file-is-being-downloaded-golang
//
// passthru wraps an existing io.Reader or io.Writer.
// It simply forwards the Read() or Write() call, while displaying
// the results from individual calls to it.
type passthru struct {
	io.Reader
	io.Writer
	total    uint64 // Total # of bytes transferred
	length   uint64 // Expected length
	progress float64
}

// Write 'overrides' the underlying io.Reader's Read method.
// This is the one that will be called by io.Copy(). We simply
// use it to keep track of byte counts and then forward the call.
// NOTE: Print a new line after any commands which use this io.Reader.
func (pt *passthru) Read(b []byte) (int, error) {
	n, err := pt.Reader.Read(b)
	if n <= 0 || doQuiet {
		return n, err
	}
	pt.total += uint64(n)
	pt.Print()
	return n, err
}

// Write 'overrides' the underlying io.Writer's Write method.
// This is the one that will be called by io.Copy(). We simply
// use it to keep track of byte counts and then forward the call.
// NOTE: Print a new line after any commands which use this io.Writer.
func (pt *passthru) Write(b []byte) (int, error) {
	n, err := pt.Writer.Write(b)
	if n <= 0 || doQuiet {
		return n, err
	}
	pt.total += uint64(n)
	pt.Print()
	return n, err
}

// Print progress.
func (pt *passthru) Print() {
	percentage := float64(pt.total) / float64(pt.length) * float64(100)
	percent := int(percentage)
	if percentage-pt.progress < 1 && percent < 99 {
		return
	}

	total := fmtSize(pt.total)
	goal := fmtSize(pt.length)
	ratio := fmt.Sprintf("%.3f", float64(pt.total)/float64(pt.length))

	fmt.Printf(
		"\r%v\r  %v%%   %v / %v = %v",
		strings.Repeat(" ", 70),
		percent, total, goal, ratio)

	pt.progress = percentage
}

// Slight variation of bytefmt.ByteSize() from:
// https://github.com/pivotal-golang/bytefmt/blob/master/bytes.go
const (
	BYTE     = 1.0
	KIBIBYTE = 1000 * BYTE
	MEBIBYTE = 1000 * KIBIBYTE
	GIBIBYTE = 1000 * MEBIBYTE
	TEBIBYTE = 1000 * GIBIBYTE
)

func fmtSize(bytes uint64) string {
	unit := ""
	value := float64(bytes)

	switch {
	case bytes >= TEBIBYTE:
		unit = "TiB"
		value = value / TEBIBYTE
	case bytes >= GIBIBYTE:
		unit = "GiB"
		value = value / GIBIBYTE
	case bytes >= MEBIBYTE:
		unit = "MiB"
		value = value / MEBIBYTE
	case bytes >= KIBIBYTE:
		unit = "KiB"
		value = value / KIBIBYTE
	case bytes >= BYTE:
		unit = "Bytes"
	case bytes == 0:
		return "0"
	}

	stringValue := fmt.Sprintf("%.1f", value)
	return concat(stringValue, " ", unit)
}

// Create a file if it doesn't exist. Otherwise, just open it.
func create(filename string, mode os.FileMode) (*os.File, error) {
	genUnusedFilename(&filename)
	file, err := os.OpenFile(
		filename,
		os.O_RDWR|os.O_CREATE|os.O_APPEND,
		mode,
	)
	return file, err
}

// Modify a filename to one that has not been used by the system.
func genUnusedFilename(filename *string) {
	if !exists(*filename) {
		return
	}
	base, ext := splitExt(*filename)
	for i := 1; i < 20091110230000; i++ {
		testname := concat(base, "(", strconv.Itoa(i), ")", ext)
		if exists(testname) {
			continue
		}
		*filename = testname
		return
	}
}

// Split the extension off a filename.
// Return the basename and the extension.
func splitExt(filename string) (base, ext string) {
	base = filepath.Clean(filename)
	for {
		testext := filepath.Ext(base)
		if testext == "" {
			return
		}
		if mime.TypeByExtension(testext) == "" {
			switch testext {
			case ".tar", ".sz", ".tar.sz":
				break
			default:
				return
			}
		}
		ext = concat(testext, ext)
		base = strings.TrimSuffix(base, testext)
	}
}

// Check whether a file exists.
func exists(filename string) bool {
	if _, err := os.Stat(filename); err == nil {
		return true
	}
	return false
}

type snapper struct {
	snappyWriter *snappy.Writer
	bufioWriter  *bufio.Writer
}

// Compress a file to a snappy archive.
// If the source file is too large for the system to handle,
//   the snapSafe() function runs instead.
// Compared to snap(), the compression ratio for this function is lower.
func snap(src *os.File) (dst *os.File, err error) {
	srcInfo, err := src.Stat()
	if err != nil {
		return
	}

	// Make sure existing files are not overwritten.
	dstName := concat(src.Name(), ".sz")
	genUnusedFilename(&dstName)

	// Create the destination file.
	if !doQuiet {
		fmt.Println(dstName)
	}
	dst, err = create(dstName, srcInfo.Mode())
	if err != nil {
		return
	}

	// If this function encounters an error,
	//   run the snapSafe() function instead.
	// Otherwise, re-open the new, compressed file.
	defer func() {
		switch err {
		case nil:
			dst, err = os.Open(dstName)
		default:
			dst, err = snapSafe(src)
		}
	}()

	// Read the contents of the source file.
	srcContents, err := ioutil.ReadAll(src)
	if err != nil {
		return
	}

	// Prepare to turn the destination file into a snappy file.
	pt := &passthru{
		Writer: dst,
		length: uint64(srcInfo.Size()),
	}
	defer func() { pt.Writer = nil }()
	szWriter := snappy.NewWriter(pt)
	defer szWriter.Reset(nil)

	// Write the source file's contents to the new snappy file.
	if !doQuiet {
		defer fmt.Println()
	}
	_, err = szWriter.Write(srcContents)
	if err != nil {
		return
	}
	return
}

// Compress a file to a snappy archive.
// This function runs if the source file is too large
//   for the snap() function above.
// Compared to snap(), the compression ratio for this function is lower.
func snapSafe(src *os.File) (dst *os.File, err error) {
	srcInfo, err := src.Stat()
	if err != nil {
		return
	}

	// Make sure existing files are not overwritten.
	dstName := concat(src.Name(), ".sz")
	genUnusedFilename(&dstName)
	if !doQuiet {
		fmt.Println(dstName)
	}

	// Create the destination file.
	dst, err = create(dstName, srcInfo.Mode())
	if err != nil {
		return
	}

	// Remember to re-open the compressed file  after it has been written.
	defer func() {
		if err == nil {
			dst, err = os.Open(dstName)
		}
	}()

	// Set up a *passthru writer in order to print progress.
	pt := &passthru{
		Writer: dst,
		length: uint64(srcInfo.Size()),
	}
	defer func() { pt.Writer = nil }()

	// Set up a snappy writer.
	sz := &snapper{
		snappyWriter: snappy.NewWriter(pt),
		bufioWriter:  bufio.NewWriter(nil),
	}
	szb := sz.bufioWriter
	szw := sz.snappyWriter
	defer szw.Reset(nil)

	// Write the source file's contents to the new snappy file.
	if !doQuiet {
		defer fmt.Println()
	}
	szb.Reset(szw)
	defer szb.Reset(nil)
	_, err = io.Copy(szb, src)
	src.Close()
	if err != nil {
		return
	}
	err = szb.Flush()
	if err != nil {
		return
	}
	return
}

// Decompress a snappy archive.
func unsnap(src *os.File) (dst *os.File, err error) {
	srcInfo, err := src.Stat()
	if err != nil {
		return
	}
	srcName := srcInfo.Name()

	// Make sure existing files are not overwritten.
	dstName := strings.TrimSuffix(srcName, ".sz")
	if dstName == srcName {
		dstName = concat(srcName, "-uncompressed")
	}
	genUnusedFilename(&dstName)
	if !doQuiet {
		fmt.Println(srcName)
	}

	// Create the destination file.
	dst, err = create(dstName, srcInfo.Mode())
	if err != nil {
		return
	}
	// Remember to re-open the uncompressed file after it has been written.
	defer func() {
		if err == nil {
			dst, err = os.Open(dstName)
		}
	}()

	pt := &passthru{
		Reader: src,
		length: uint64(srcInfo.Size()),
	}
	defer func() { pt.Reader = nil }()
	szReader := snappy.NewReader(pt)
	defer szReader.Reset(nil)

	if !doQuiet {
		defer fmt.Println()
	}
	_, err = io.Copy(dst, szReader)
	if err != nil {
		return
	}
	return
}

// Extract a tar archive.
func untar(file *os.File) error {
	fi, err := file.Stat()
	if err != nil {
		return err
	}
	total := uint64(fi.Size())
	name := fi.Name()

	// Make sure existing files are not overwritten.
	originName := strings.TrimSuffix(name, ".tar")
	dstName := originName
	genUnusedFilename(&dstName)

	tr := tar.NewReader(file)

	if !doQuiet {
		fmt.Println(name)
		defer fmt.Println()
	}
	var progress uint64
	for {
		var hdr *tar.Header
		hdr, err = tr.Next()
		// Break if the end of the tar archive has been reached.
		if err == io.EOF {
			err = nil
			break
		} else if err != nil {
			break
		}

		// Make sure existing files are not overwritten.
		name := hdr.Name
		if dstName != originName {
			name = strings.Replace(name, originName, dstName, 1)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// Extract a directory.
			if err = os.MkdirAll(name, os.FileMode(hdr.Mode)); err != nil {
				break
			}

		case tar.TypeReg, tar.TypeRegA:
			// Extract a regular file.
			var w *os.File
			w, err = create(name, os.FileMode(hdr.Mode))
			if err != nil {
				break
			}
			if _, err = io.Copy(w, tr); err != nil {
				break
			}
			w.Close()

		case tar.TypeLink:
			// Extract a hard link.
			if err = os.Link(hdr.Linkname, name); err != nil {
				break
			}

		case tar.TypeSymlink:
			// Extract a symlink.
			if err = os.Symlink(hdr.Linkname, name); err != nil {
				break
			}

		default:
			// Continue loop without printing progress.
			continue
		}

		// Print progress.
		if doQuiet || hdr.Size == int64(0) {
			continue
		}
		progress = progress + uint64(hdr.Size)
		percent := int(float64(progress) / float64(total) * float64(100))
		fmt.Printf(
			"\r%v\r  %v%%   %v / %v files",
			strings.Repeat(" ", 70),
			percent, fmtSize(progress), fmtSize(total),
		)
	}

	if err != nil {
		return fmt.Errorf("%v\nFailed to extract %v", err, name)
	}
	return nil
}

// Append a "/" to a string if it doesn't have one already.
func fmtDir(name *string) {
	s := string(filepath.Separator)
	if !strings.HasSuffix(*name, s) {
		*name = concat(*name, s)
	}
}

// Return the total size in bytes and number of files under a directory.
func dirSize(dir string) (b int64, i int) {
	filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		b += fi.Size()
		i += 1
		return nil
	})
	return
}

// https://github.com/docker/docker/blob/master/pkg/archive/archive.go
type tarAppender struct {
	tarWriter   *tar.Writer
	bufioWriter *bufio.Writer
	// Map inodes to hardlinks.
	hardLinks map[uint64]string
}

// https://github.com/docker/docker/blob/master/pkg/archive/archive.go
// Create a tar archive of a file.
func tarDir(dir *os.File) (dst *os.File, err error) {
	dirInfo, err := dir.Stat()
	if err != nil {
		return
	}
	// Create the destination file.
	dirName := dir.Name()
	dstName := concat(dirName, ".tar")
	genUnusedFilename(&dstName)
	dst, err = create(dstName, dirInfo.Mode())
	if err != nil {
		return
	}
	// Remember to re-open the tar archive after it has been written.
	defer func() {
		if err == nil {
			dst, err = os.Open(dstName)
		}
	}()

	var dstWriter io.WriteCloser = dst
	ta := &tarAppender{
		tarWriter:   tar.NewWriter(dstWriter),
		bufioWriter: bufio.NewWriter(nil),
		hardLinks:   make(map[uint64]string),
	}

	// Remember to close the tarWriter.
	defer func() {
		err = ta.tarWriter.Close()
	}()

	// Walk through the directory.
	// Add a header to the tar archive for each file encountered.
	var total, progress int
	if !doQuiet {
		_, total = dirSize(dirName)
		fmt.Println(dstName)
		defer fmt.Println()
	}
	err = filepath.Walk(dirName, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		err = ta.add(path, path)
		if err != nil {
			return err
		}

		if doQuiet {
			return nil
		}
		progress += 1
		percent := int(float64(progress) / float64(total) * float64(100))
		fmt.Printf(
			"\r%v\r  %v%%   %v / %v files",
			strings.Repeat(" ", 50),
			percent, progress, total,
		)
		return nil
	})

	return
}

// https://github.com/docker/docker/blob/master/pkg/archive/archive.go
// Add a file [as a header] to a tar archive.
func (ta *tarAppender) add(path, name string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}

	// If the file is a symlink, find its target.
	var link string
	if fi.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	}

	// Create the tar header.
	hdr, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}

	// Set the header name.
	// If the file is a directory, add a trailing "/".
	if fi.Mode()&os.ModeDir != 0 {
		fmtDir(&name)
	}
	hdr.Name = name

	// Check if the file has hard links.
	nlink, inode, err := tarSetHeader(hdr, fi.Sys())
	if err != nil {
		return err
	}

	// If any other regular files link to the same inode as this file,
	//   prepare to treat it as a "hardlink" in the header.
	// If the tar archive contains another hardlink to this file's inode,
	//   set it as a "hardlink" in the tar header.
	// Otherwise, treat it as a regular file.
	if fi.Mode().IsRegular() && nlink > 1 {
		// If this file is NOT the first found hardlink to this inode,
		//   set the previously found hardlink as its 'Linkname'.
		if oldpath, ok := ta.hardLinks[inode]; ok {
			hdr.Typeflag = tar.TypeLink
			hdr.Linkname = oldpath
			// Set size to 0 when not adding additional inodes.
			//   Otherwise, the writer's math will not add up correctly.
			hdr.Size = 0

			// If this file IS the first hardlink to this inode,
			//   note the file with its inode and treat it as a regular file.
			// It will become the 'Linkname' for another hardlink
			//   further down in the archive.
		} else {
			ta.hardLinks[inode] = name
		}
	}

	// Find any security.capability xattrs and set the header accordingly.
	capability, _ := lgetxattr(path, "security.capability")
	if capability != nil {
		hdr.Xattrs = make(map[string]string)
		hdr.Xattrs["security.capability"] = string(capability)
	}

	// Write the header.
	tw := ta.tarWriter
	if err = tw.WriteHeader(hdr); err != nil {
		return err
	}

	// If the file is a regular one,
	//   i.e., not a symlink, directory, or hardlink,
	//   write the file's contents to the buffer.
	if hdr.Typeflag == tar.TypeReg {
		tb := ta.bufioWriter
		file, err := os.Open(path)
		if err != nil {
			return err
		}

		tb.Reset(tw)
		defer tb.Reset(nil)
		_, err = io.Copy(tb, file)
		file.Close()
		if err != nil {
			return err
		}
		err = tb.Flush()
		if err != nil {
			return err
		}
	}
	return nil
}

// https://github.com/docker/docker/blob/master/pkg/archive/archive_unix.go
// Add a file's device major and minor numbers
//   to the file's header within a tar archive.
// Return the file's inode and the number of hardlinks to that inode.
func tarSetHeader(hdr *tar.Header, stat interface{}) (nlink uint32, inode uint64, err error) {
	s, ok := stat.(*syscall.Stat_t)
	if !ok {
		err = fmt.Errorf("cannot convert stat value to syscall.Stat_t")
		return
	}

	nlink = uint32(s.Nlink)
	inode = uint64(s.Ino)

	// Currently go does not fil in the major/minors
	if s.Mode&syscall.S_IFBLK != 0 || s.Mode&syscall.S_IFCHR != 0 {
		hdr.Devmajor = int64(devmajor(uint64(s.Rdev)))
		hdr.Devminor = int64(devminor(uint64(s.Rdev)))
	}

	return
}

// https://github.com/docker/docker/blob/master/pkg/archive/archive_unix.go
// Return the device major number of system data from syscall.Stat_t.Rdev.
func devmajor(device uint64) uint64 {
	return (device >> 8) & 0xfff
}

// https://github.com/docker/docker/blob/master/pkg/archive/archive_unix.go
// Return the device minor number of system data from syscall.Stat_t.Rdev.
func devminor(device uint64) uint64 {
	return (device & 0xff) | ((device >> 12) & 0xfff00)
}

// https://github.com/docker/docker/blob/master/pkg/system/xattrs_linux.go
// Get the underlying data for an xattr of a file.
// Return a nil slice and nil error if the xattr is not set.
// Other than that, I have no idea how this function works.
func lgetxattr(path string, attr string) ([]byte, error) {
	pathBytes, err := syscall.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	attrBytes, err := syscall.BytePtrFromString(attr)
	if err != nil {
		return nil, err
	}

	dest := make([]byte, 128)
	destBytes := unsafe.Pointer(&dest[0])
	sz, _, errno := syscall.Syscall6(
		syscall.SYS_LGETXATTR,
		uintptr(unsafe.Pointer(pathBytes)),
		uintptr(unsafe.Pointer(attrBytes)),
		uintptr(destBytes),
		uintptr(len(dest)),
		0,
		0,
	)
	if errno == syscall.ENODATA {
		return nil, nil
	}
	if errno == syscall.ERANGE {
		dest = make([]byte, sz)
		destBytes := unsafe.Pointer(&dest[0])
		sz, _, errno = syscall.Syscall6(
			syscall.SYS_LGETXATTR,
			uintptr(unsafe.Pointer(pathBytes)),
			uintptr(unsafe.Pointer(attrBytes)),
			uintptr(destBytes),
			uintptr(len(dest)),
			0,
			0,
		)
	}
	if errno != 0 {
		return nil, errno
	}

	return dest[:sz], nil
}
