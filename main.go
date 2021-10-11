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
			return fmt.Errorf("list packages: %w", err)
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
				case strings.Contains(imp, "cmd"), strings.Contains(imp, "internal"):
					subPkgs, err := listPackages(ctx, flagSrc, imp)
					if err != nil {
						return fmt.Errorf("list packages: %w", err)
					}
					packages = append(packages, subPkgs...)

				default:
					fmt.Printf("ignore: %s\n", imp)
				}
			}
		}

		for _, p := range packages {
			subPkgs, err := listPackages(ctx, flagSrc, p.Dir)
			if err != nil {
				return fmt.Errorf("list packages: %w", err)
			}

			for _, subPkg := range subPkgs {
				if err := copyInternal(subPkg); err != nil {
					return fmt.Errorf("copy internal: %w", err)
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
		return nil, fmt.Errorf("create StdoutPipe: %w", err)
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
		return nil, fmt.Errorf("start cmd: %w", err)
	}

	dec := json.NewDecoder(stdout)
	for dec.More() {
		var pkg Package
		if err := dec.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("decode json: %w", err)
		}
		pkgs = append(pkgs, &pkg)
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("wait cmd: %w", err)
	}

	return pkgs, nil
}

func copyInternal(pkg *Package) error {
	files := sourceFiles(pkg)
	for _, file := range files {
		if file == "zbootstrap.go" { // zbootstrap.go is created by bootstrap
			continue
		}

		dir, filename := filepath.Split(file)
		dir = strings.TrimPrefix(dir, gorootSrc)
		dir = strings.ReplaceAll(dir, "cmd", "")
		dir = strings.ReplaceAll(dir, "internal", "")

		dstPath := filepath.Join(flagDist, dir)
		fmt.Printf("dstPath: %s\n", dstPath)

		data, err := readFile(file)
		if err != nil {
			return err
		}

		if err := writeFile(dstPath, filename, data); err != nil {
			return fmt.Errorf("write file: %w", err)
		}
	}

	return nil
}

func sourceFiles(pkg *Package) (files []string) {
	fileLists := [...][]string{
		pkg.GoFiles,
		pkg.TestGoFiles,
		pkg.XTestGoFiles,
		pkg.IgnoredGoFiles,
	}

	for _, fileList := range fileLists {
		for _, file := range fileList {
			files = append(files, filepath.Join(pkg.Dir, file))
		}
	}

	return files
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", path, err)
	}

	body := string(data)
	body = strings.ReplaceAll(body, `"cmd`, `"`+flagModule)
	body = strings.ReplaceAll(body, `"internal`, `"`+flagModule)
	body = strings.ReplaceAll(body, `/internal`, ``)

	return body, nil
}

func writeFile(dir, name, body string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	imports.LocalPrefix = flagModule
	data, err := imports.Process(name, []byte(body), &imports.Options{
		TabWidth:  8,
		TabIndent: true,
		Comments:  true,
	})
	if err != nil {
		return fmt.Errorf("process goimports: %w", err)
	}

	filename := filepath.Join(dir, name)
	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("write %s file: %w", filename, err)
	}

	return nil
}
