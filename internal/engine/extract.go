package engine

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
)

// extractFlat unpacks an archive, writing every regular file into destDir by its
// base name. Engine archives ship llama-server + shared libs that all belong in
// one directory, so flattening is correct and keeps them side by side.
func extractFlat(archivePath, destDir, kind string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	if kind == "zip" {
		return extractZipFlat(archivePath, destDir)
	}
	return extractTarGzFlat(archivePath, destDir)
}

func writeFlat(destDir, name string, r io.Reader, mode os.FileMode) error {
	base := filepath.Base(name)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return nil
	}
	out := filepath.Join(destDir, base)
	f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode|0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func extractZipFlat(src, destDir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFlat(destDir, f.Name, rc, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGzFlat(src, destDir string) error {
	f, err := os.Open(src)
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
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := writeFlat(destDir, h.Name, tr, os.FileMode(h.Mode)); err != nil {
			return err
		}
	}
	return nil
}
