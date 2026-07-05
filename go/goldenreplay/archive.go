package goldenreplay

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path"
	"path/filepath"
)

// loadTarGzIndex decompresses a gzip'd tar archive fully into memory, keyed by each
// regular file entry's in-archive relative path (e.g. "get__ai__aiview/body.bin",
// "aiview.json"). Used for archives whose contents are only ever read as bytes —
// golden.tar.gz and fixtures/seeds.tar.gz — so no directory ever needs to exist on disk
// for these; readGolden and the seed-delta lookup are just map reads.
func loadTarGzIndex(archivePath string) (map[string][]byte, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[path.Clean(hdr.Name)] = b
	}
	return out, nil
}

// extractTarGz decompresses archivePath into destDir as a real directory tree,
// preserving symlinks exactly as stored rather than dereferencing them — chdb's Atomic
// engine maps metadata/default -> ../store/<uuid> via a relative symlink, and resolving
// it into a plain file copy at extract time breaks that mapping (see the former
// copyDir, which this replaces, for the same rationale). Used for archives that DO need
// to exist as a real directory: fixtures/base.tar.gz (chdb opens a directory) and
// fixtures/upstream.tar.gz (the server reads mock-upstream files off a directory path).
func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel := path.Clean(hdr.Name)
		if rel == "." {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode)
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
