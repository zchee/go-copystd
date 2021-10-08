// Copyright 2021 The go-copystd Authors
// SPDX-License-Identifier: BSD-3-Clause

// Command go-copystd copies Go stdlib internal package along with its dependency packages.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/tools/imports"
)

type stringsFlag []string

func (s *stringsFlag) String() string {
	return fmt.Sprint(*s)
}

func (s *stringsFlag) Set(value string) error {
	if len(*s) > 0 {
		return errors.New("flag already set")
	}
	for _, str := range strings.Split(value, ",") {
		*s = append(*s, str)
	}

	return nil
}

var (
	flagPackages stringsFlag
	flagModule   string
	flagSrc      string
	flagDist     string
)

var gorootSrc = filepath.Join(runtime.GOROOT(), "src")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	flag.Var(&flagPackages, "package", "comma separated copy stdlib packages")
	flag.StringVar(&flagModule, "module", "", "module import path")
	flag.StringVar(&flagSrc, "src", runtime.GOROOT(), "src directory")
	flag.StringVar(&flagDist, "dst", ".", "dist directory")
	flag.Parse()

	imports.LocalPrefix = flagModule

	ctx := context.Background()
	for _, pkg := range flagPackages {
		listPkgs, err := listPackages(ctx, flagSrc, pkg)
		if err != nil {
			return fmt.Errorf("listPackages: %w", err)
		}

		var packages []*Package
		for _, listPkg := range listPkgs {
			if _, err := os.Stat(listPkg.Dir); err != nil && os.IsNotExist(err) {
				if listPkg.Dir != "" {
					fmt.Printf("[WARN]: %s is not exists, continue\n", listPkg.Dir)
				}
				continue
			}

			packages = append(packages, listPkg)
			for _, imp := range listPkg.Imports {
				switch {
				case strings.HasPrefix(imp, "cmd"), strings.HasPrefix(imp, "internal"):
					subPkgs, err := listPackages(ctx, flagSrc, imp)
					if err != nil {
						return err
					}
					packages = append(packages, subPkgs...)
				default:
					fmt.Printf("ignore: %s\n", imp)
				}
			}
		}

		for _, p := range packages {
			dir := filepath.Join(flagDist, strings.TrimPrefix(p.Dir, gorootSrc))
			if _, err := os.Stat(dir); err != nil && !os.IsNotExist(err) {
				continue
			}

			subPkgs, err := listPackages(ctx, flagSrc, p.Dir)
			if err != nil {
				return err
			}

			for _, subPkg := range subPkgs {
				if err := copyInternal(subPkg); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// listPackages is a wrapper for 'go list -json -e', which can take arbitrary
// environment variables and arguments as input. The working directory can be
// fed by adding $PWD to env; otherwise, it will default to the current
// directory.
//
// Since -e is used, the returned error will only be non-nil if a JSON result
// could not be obtained. Such examples are if the Go command is not installed,
// or if invalid flags are used as arguments.
//
// Errors encountered when loading packages will be returned for each package,
// in the form of PackageError. See 'go help list'.
func listPackages(ctx context.Context, src string, args ...string) (pkgs []*Package, finalErr error) {
	goArgs := append([]string{"list", "-json", "-e"}, args...)
	cmd := exec.CommandContext(ctx, "go", goArgs...)
	cmd.Env = append(os.Environ(), []string{"PWD=" + src}...)
	cmd.Dir = src

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	defer func() {
		if finalErr != nil && stderrBuf.Len() > 0 {
			// TODO: wrap? but the format is backwards, given that
			// stderr is likely multi-line
			finalErr = fmt.Errorf("%w\n%s", finalErr, stderrBuf.Bytes())
		}
	}()

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(stdout)
	for dec.More() {
		var pkg Package
		if err := dec.Decode(&pkg); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, &pkg)
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}

	return pkgs, nil
}

func copyInternal(pkg *Package) error {
	for _, path := range sourceFiles(pkg) {
		// zbootstrap.go is created by bootstrap
		if path == "zbootstrap.go" {
			continue
		}

		body := readFile(path)
		name := filepath.Base(path)

		for imppath, fixpath := range map[string]string{
			filepath.Join(gorootSrc, "cmd", "asm", "internal"):     filepath.Join(gorootSrc, "asm"),
			filepath.Join(gorootSrc, "cmd", "compile", "internal"): filepath.Join(gorootSrc, "compile"),
			filepath.Join(gorootSrc, "cmd", "go", "internal"):      filepath.Join(gorootSrc, "go"),
			filepath.Join(gorootSrc, "cmd", "link", "internal"):    filepath.Join(gorootSrc, "link"),
			filepath.Join(gorootSrc, "cmd", "internal"):            filepath.Join(gorootSrc),
			filepath.Join(gorootSrc, "internal"):                   filepath.Join(gorootSrc),
		} {
			if strings.HasPrefix(pkg.Dir, imppath) {
				dstpath := strings.ReplaceAll(pkg.Dir, imppath, fixpath)

				if err := writeFile(strings.TrimPrefix(dstpath, gorootSrc), name, body); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func sourceFiles(pkg *Package) (paths []string) {
	var files []string
	for _, list := range [...][]string{
		pkg.GoFiles,
		pkg.TestGoFiles,
		pkg.XTestGoFiles,
		pkg.IgnoredGoFiles,
	} {
		for _, name := range list {
			files = append(files, filepath.Join(pkg.Dir, name))
		}
	}

	return files
}

func readFile(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}

	sep := string(os.PathSeparator)
	str := string(body)
	str = strings.ReplaceAll(str, "cmd/asm/internal"+sep, filepath.Join(flagModule, "asm"+sep))
	str = strings.ReplaceAll(str, "cmd/compile/internal"+sep, filepath.Join(flagModule, "compile"+sep))
	str = strings.ReplaceAll(str, "cmd/go/internal"+sep, filepath.Join(flagModule, "go"+sep))
	str = strings.ReplaceAll(str, "cmd/link/internal"+sep, filepath.Join(flagModule, "link"+sep))
	str = strings.ReplaceAll(str, "cmd/internal"+sep, flagModule+sep)
	str = strings.ReplaceAll(str, "internal"+sep, flagModule+sep)

	return str
}

func writeFile(dir, name, body string) error {
	if err := os.MkdirAll(filepath.Join(flagDist, dir), 0o755); err != nil {
		return err
	}

	data, err := imports.Process(name, []byte(body), &imports.Options{
		TabWidth:  8,
		TabIndent: true,
		Comments:  true,
	})
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(flagDist, dir, name), data, 0o600); err != nil {
		return err
	}

	return nil
}
