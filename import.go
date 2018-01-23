package stacker

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
)

func fileCopy(dest string, source string) error {
	s, err := os.Open(source)
	if err != nil {
		return err
	}
	defer s.Close()

	fi, err := s.Stat()
	if err != nil {
		return err
	}

	d, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer d.Close()

	err = d.Chmod(fi.Mode())
	if err != nil {
		return err
	}

	_, err = io.Copy(d, s)
	return err
}

func Import(c StackerConfig, name string, imports []string) error {
	dir := path.Join(c.StackerDir, "imports", name)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	for _, i := range imports {
		url, err := url.Parse(i)
		if err != nil {
			return err
		}

		// It's just a path, let's copy it to .stacker.
		if url.Scheme == "" {
			e1, err := os.Stat(i)
			if err != nil {
				return err
			}

			needsCopy := false
			dest := path.Join(dir, path.Base(url.Path))
			e2, err := os.Stat(dest)
			if err != nil {
				needsCopy = true
			} else {
				differ, err := filesDiffer(i, e1, dest, e2)
				if err != nil {
					return err
				}

				needsCopy = differ
			}

			if needsCopy {
				fmt.Printf("copying %s\n", i)
				if err := fileCopy(dest, i); err != nil {
					return err
				}
			} else {
				fmt.Println("using cached copy of", i)
			}
		} else {
			// otherwise, we need to download it
			_, err = download(dir, i)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
