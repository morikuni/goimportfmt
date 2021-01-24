package goimportfmt

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa/interp/testdata/src/errors"
)

func DetectModulePath(file string) (string, error) {
	fi, err := os.Stat(file)
	if err != nil && os.IsNotExist(err) {
		return "", errors.New("file doest not exist")
	}

	if fi.IsDir() {
		return "", errors.New("directory is not supported")
	}

	dir, err := filepath.Abs(filepath.Dir(file))
	if err != nil {
		return "", errors.New("could not get absolute path")
	}

	cfg := &packages.Config{
		Dir:  dir,
		Mode: packages.NeedModule,
	}
	pkgs, err := packages.Load(cfg, file)
	if err != nil {
		return "", fmt.Errorf("failed to load package: %w", err)
	}

	var pkgErr error
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			pkgErr = fmt.Errorf("package error: %s", e.Msg)
		}
	})
	if pkgErr != nil {
		// TODO: should return error or print error?
		return "", nil
	}

	if len(pkgs) == 0 {
		return "", errors.New("package not found")
	}
	if len(pkgs) > 1 {
		return "", errors.New("found 2 or more packages")
	}

	pkg := pkgs[0]

	if pkg.Module == nil {
		// not using module
		return "", nil
	}

	if pkg.Module.Path == "command-line-arguments" {
		// Path will be "command-line-arguments" when not in module.
		return "", nil
	}

	return pkg.Module.Path, nil
}

type Import interface {
	Name() string
	Path() string
	Docs() []string
	Comment() string
}

type GroupedImports map[int][]Import

type FormatFunc func(Context, []Import) GroupedImports

type Option struct {
	// ModulePath specifies module path of the src.
	// If empty, automatically detect module path from go.mod.
	ModulePath string
}

type Context struct {
	ModulePath string
}
