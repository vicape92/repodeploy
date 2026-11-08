// Package upload owns manually uploaded source archives: safe extraction,
// normalisation into content-addressed tar.gz files, retention, and the
// client-side file selection and packing used by "repodeploy up". It depends
// only on the standard library.
package upload

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultMaxBytes is the default largest accepted upload archive.
const DefaultMaxBytes int64 = 200 << 20

// Limits bounds what an archive may expand to.
type Limits struct {
	MaxExpanded int64
	MaxEntries  int
}

// DefaultLimits allows five times the upload size and 50 000 entries.
func DefaultLimits(maxBytes int64) Limits {
	return Limits{MaxExpanded: 5 * maxBytes, MaxEntries: 50000}
}

// unlimited re-extracts archives that were validated when they were stored.
var unlimited = Limits{MaxExpanded: 1 << 62, MaxEntries: 1 << 30}

// Error is a problem with the uploaded archive itself.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func invalid(format string, a ...any) error { return &Error{Message: fmt.Sprintf(format, a...)} }

type extractor struct {
	root    string
	lim     Limits
	written int64
	entries int
}

// extract unpacks a zip or gzip-compressed tar archive into dest, which must
// not exist yet. Unsafe entries abort the extraction.
func extract(archive, dest string, lim Limits) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err = os.Mkdir(dest, 0755); err != nil {
		return err
	}
	x := &extractor{root: dest, lim: lim}
	switch {
	case n == 4 && string(magic) == "PK\x03\x04":
		info, err := f.Stat()
		if err != nil {
			return err
		}
		return x.zip(f, info.Size())
	case n >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		return x.tarGz(f)
	default:
		return invalid("upload a .zip or .tar.gz archive")
	}
}

func (x *extractor) zip(r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return invalid("the zip archive is damaged: %v", err)
	}
	for _, f := range zr.File {
		mode := f.Mode()
		switch {
		case mode.IsDir():
			err = x.dir(f.Name)
		case mode&fs.ModeSymlink != 0:
			err = x.zipSymlink(f)
		case mode.IsRegular():
			var rc io.ReadCloser
			if rc, err = f.Open(); err != nil {
				return invalid("cannot read %s: %v", f.Name, err)
			}
			err = x.file(f.Name, rc, mode)
			rc.Close()
		default:
			err = invalid("%s is not a regular file, directory or symlink", f.Name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (x *extractor) zipSymlink(f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return invalid("cannot read %s: %v", f.Name, err)
	}
	defer rc.Close()
	target, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return invalid("cannot read %s: %v", f.Name, err)
	}
	return x.symlink(f.Name, string(target))
}

func (x *extractor) tarGz(r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return invalid("the tar.gz archive is damaged: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return invalid("the tar.gz archive is damaged: %v", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = x.dir(h.Name)
		case tar.TypeReg:
			err = x.file(h.Name, tr, fs.FileMode(h.Mode))
		case tar.TypeSymlink:
			err = x.symlink(h.Name, h.Linkname)
		case tar.TypeXGlobalHeader:
			continue
		default:
			err = invalid("%s is not a regular file, directory or symlink", h.Name)
		}
		if err != nil {
			return err
		}
	}
}

// clean validates an entry name and returns its slash-separated relative
// path, or "" for entries that are skipped.
func clean(name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(name, "/") || (len(name) > 1 && name[1] == ':') {
		return "", invalid("%s uses an absolute path", name)
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", invalid("%s points outside the archive", name)
	}
	if cleaned == "." || strings.Split(cleaned, "/")[0] == "__MACOSX" || path.Base(cleaned) == ".DS_Store" {
		return "", nil
	}
	return cleaned, nil
}

// prepare counts the entry, creates missing parent directories and refuses
// to write through an existing symlink, so a link created by an earlier
// entry can never redirect a later one outside the root.
func (x *extractor) prepare(rel string) (string, error) {
	x.entries++
	if x.entries > x.lim.MaxEntries {
		return "", invalid("the archive has more than %d entries", x.lim.MaxEntries)
	}
	parts := strings.Split(rel, "/")
	current := x.root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err = os.Mkdir(current, 0755); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", invalid("%s writes through the symlink %s", rel, part)
		}
		if !info.IsDir() {
			return "", invalid("%s is inside %s, which is not a directory", rel, part)
		}
	}
	return filepath.Join(x.root, filepath.FromSlash(rel)), nil
}

func (x *extractor) dir(name string) error {
	rel, err := clean(name)
	if err != nil || rel == "" {
		return err
	}
	target, err := x.prepare(rel)
	if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		return invalid("%s appears twice in the archive", rel)
	}
	return os.Mkdir(target, 0755)
}

func (x *extractor) file(name string, r io.Reader, mode fs.FileMode) error {
	rel, err := clean(name)
	if err != nil || rel == "" {
		return err
	}
	target, err := x.prepare(rel)
	if err != nil {
		return err
	}
	perm := fs.FileMode(0644)
	if mode&0111 != 0 {
		perm = 0755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) {
		return invalid("%s appears twice in the archive", rel)
	}
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, x.lim.MaxExpanded-x.written+1))
	x.written += n
	closeErr := f.Close()
	if err != nil {
		return invalid("cannot extract %s: %v", rel, err)
	}
	if closeErr != nil {
		return closeErr
	}
	if x.written > x.lim.MaxExpanded {
		return invalid("the archive expands to more than %d MB", x.lim.MaxExpanded>>20)
	}
	return os.Chmod(target, perm)
}

func (x *extractor) symlink(name, target string) error {
	rel, err := clean(name)
	if err != nil || rel == "" {
		return err
	}
	if target == "" || strings.HasPrefix(target, "/") || strings.Contains(target, `\`) {
		return invalid("symlink %s must use a relative target", rel)
	}
	resolved := path.Clean(path.Join(path.Dir(rel), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return invalid("symlink %s points outside the archive", rel)
	}
	dest, err := x.prepare(rel)
	if err != nil {
		return err
	}
	if err = os.Symlink(target, dest); errors.Is(err, fs.ErrExist) {
		return invalid("%s appears twice in the archive", rel)
	}
	return err
}

// Pack writes a deterministic tar.gz of the listed paths (slash-separated,
// relative to root): sorted entries, zero timestamps and owners, and modes
// reduced to 0755/0644. Symlinks must stay inside root.
func Pack(root string, files []string, w io.Writer) error {
	sorted := append([]string{}, files...)
	sort.Strings(sorted)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, rel := range sorted {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		h := &tar.Header{Name: rel, ModTime: time.Unix(0, 0), Mode: 0644}
		switch {
		case info.IsDir():
			h.Typeflag, h.Name, h.Mode = tar.TypeDir, rel+"/", 0755
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			target = filepath.ToSlash(target)
			resolved := path.Clean(path.Join(path.Dir(rel), target))
			if path.IsAbs(target) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symlink %s points outside the project", rel)
			}
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, target, 0777
		case info.Mode().IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, info.Size()
			if info.Mode()&0111 != 0 {
				h.Mode = 0755
			}
		default:
			return fmt.Errorf("%s is not a regular file, directory or symlink", rel)
		}
		if err = tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, h.Size)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
