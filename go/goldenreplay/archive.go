package goldenreplay

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
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

	// Symlink entries are deferred to a second pass, after every directory and regular
	// file has been created, so isRelSafe's EvalSymlinks call always has a real
	// filesystem tree to resolve pre-existing symlinks against (see isRelSafe).
	type symlinkEntry struct {
		name, linkname string
	}
	var symlinks []symlinkEntry

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Reject any entry name containing ".." before it is used in any file-system
		// operation — the barrier pattern go/zipslip's own documentation recommends
		// checking ahead of use, rather than validating the joined result after the fact.
		if strings.Contains(hdr.Name, "..") {
			return fmt.Errorf("tar entry %q must not contain \"..\"", hdr.Name)
		}
		rel := path.Clean(hdr.Name)
		if rel == "." {
			continue
		}
		target, err := safeJoin(destDir, rel)
		if err != nil {
			return fmt.Errorf("tar entry %q: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// hdr.Linkname legitimately contains ".." for any relative symlink that walks
			// up a directory (e.g. chdb's Atomic engine linking "metadata/default" to
			// "../store/<uuid>") — unlike an archive entry NAME, a target is validated by
			// resolving where it actually lands (isRelSafeTarget below), not by rejecting
			// ".." outright.
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("tar entry %q: symlink target %q must be relative", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			symlinks = append(symlinks, symlinkEntry{name: rel, linkname: hdr.Linkname})
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

	if len(symlinks) > 0 {
		// Resolve destDir itself once (e.g. macOS's /tmp -> /private/tmp, or /var ->
		// /private/var, which t.TempDir() paths go through): every EvalSymlinks-resolved
		// path below is compared against this resolved base, not the unresolved destDir,
		// or a legitimate entry inside an unresolved-but-symlinked destDir would look like
		// it escapes.
		realBase, err := filepath.EvalSymlinks(destDir)
		if err != nil {
			return fmt.Errorf("resolving destination directory: %w", err)
		}
		for _, sl := range symlinks {
			target, err := safeJoin(destDir, sl.name)
			if err != nil {
				return fmt.Errorf("symlink %q: %w", sl.name, err)
			}
			// isRelSafe resolves any symlinks ALREADY extracted (e.g. chdb's Atomic engine
			// links one directory level before another) before checking the result stays
			// within destDir — a syntactic filepath.Rel check alone (this function's
			// previous approach) can't detect an escape built from a chain of
			// individually-safe-looking relative links, which is exactly what
			// go/unsafe-unzip-symlink guards against.
			if !isRelSafe(sl.name, realBase) {
				return fmt.Errorf("tar entry %q: resolved path escapes destination directory", sl.name)
			}
			if !isRelSafeTarget(sl.name, sl.linkname, realBase) {
				return fmt.Errorf("tar entry %q: symlink target %q escapes destination directory", sl.name, sl.linkname)
			}
			if err := os.Symlink(sl.linkname, target); err != nil {
				return err
			}
		}
	}
	return nil
}

// safeJoin joins destDir and rel (a tar entry's cleaned relative path) and rejects the
// result if it escapes destDir — a "zip slip" guard against a crafted archive entry name
// like "../../etc/passwd" writing outside the intended extraction directory.
func safeJoin(destDir, rel string) (string, error) {
	target := filepath.Join(destDir, filepath.FromSlash(rel))
	base := filepath.Clean(destDir)
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes destination directory: %q", rel)
	}
	return target, nil
}

// isRelSafe reports whether rel (a path relative to base) resolves — after following any
// symlinks already extracted along the way — to a location still inside base. Unlike a
// purely syntactic filepath.Rel check on the unresolved path, this follows pre-existing
// symlinks first (filepath.EvalSymlinks), so a chain such as "subdir/parent -> .." followed
// by "escape -> subdir/parent/.." can't walk outside base even though each individual link
// looks locally safe when checked in isolation.
func isRelSafe(rel, base string) bool {
	if filepath.IsAbs(rel) {
		return false
	}
	joined := filepath.Join(base, rel)
	realDir, err := filepath.EvalSymlinks(filepath.Dir(joined))
	if err != nil {
		return false
	}
	realpath := filepath.Join(realDir, filepath.Base(joined))
	relpath, err := filepath.Rel(base, realpath)
	return err == nil && relpath != ".." && !strings.HasPrefix(relpath, ".."+string(os.PathSeparator))
}

// isRelSafeTarget reports whether a symlink at rel (relative to base) pointing at linkname
// (relative to the symlink's own directory, as os.Symlink/the filesystem interpret it)
// resolves to a location still inside base, following any pre-existing symlinks the same
// way isRelSafe does.
func isRelSafeTarget(rel, linkname, base string) bool {
	if filepath.IsAbs(linkname) {
		return false
	}
	linkDir := filepath.Dir(filepath.Join(base, rel))
	realLinkDir, err := filepath.EvalSymlinks(linkDir)
	if err != nil {
		return false
	}
	joined := filepath.Join(realLinkDir, linkname)
	realDir, err := filepath.EvalSymlinks(filepath.Dir(joined))
	if err != nil {
		return false
	}
	realpath := filepath.Join(realDir, filepath.Base(joined))
	relpath, err := filepath.Rel(base, realpath)
	return err == nil && relpath != ".." && !strings.HasPrefix(relpath, ".."+string(os.PathSeparator))
}
