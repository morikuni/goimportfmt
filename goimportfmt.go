package goimportfmt

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

func Process(src io.Reader, dst io.Writer, opts ...Option) error {
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "", src, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("failed to parse file: %w", err)
	}

	is, err := loadImports(f)
	if err != nil {
		return fmt.Errorf("failed to remove imports: %w", err)
	}

	fl, err := removeImports(fs, f)
	if err != nil {
		return fmt.Errorf("failed to remove imports: %w", err)
	}

	conf := newConfig(opts)

	ctx := &Context{
		ModulePath: conf.modulePath,
	}
	w := &importInsertWriter{
		dst,
		fl,
		0,
		nil,
		conf.formatFunc(ctx, is),
	}

	err = format.Node(w, fs, f)
	if err != nil {
		return err
	}

	return nil
}

type importInsertWriter struct {
	w          io.Writer
	lineInsert int
	count      int
	buf        []byte
	imports    GroupedImports
}

func (w *importInsertWriter) Write(bs []byte) (int, error) {
	if w.count >= w.lineInsert {
		return w.w.Write(bs)
	}

	w.count += bytes.Count(bs, []byte{'\n'})
	w.buf = append(w.buf, bs...)

	if w.count < w.lineInsert {
		return len(bs), nil
	}

	var (
		count int
		idx   int
	)
	for idx = 0; idx < len(w.buf); idx++ {
		if w.buf[idx] != '\n' {
			continue
		}
		count++
		if count+1 >= w.lineInsert {
			idx++
			break
		}
	}

	var written int
	n, err := w.w.Write(w.buf[:idx])
	if err != nil {
		return 0, err
	}
	written += n

	n64, err := w.imports.WriteTo(w.w)
	if err != nil {
		return 0, err
	}
	written += int(n64)

	n, err = w.w.Write([]byte{'\n'})
	if err != nil {
		return 0, err
	}
	written += n

	n, err = w.w.Write(w.buf[idx:])
	if err != nil {
		return 0, err
	}
	written += n

	w.buf = nil // to GC

	return written, nil
}

type Option func(*config)

type config struct {
	modulePath string
	formatFunc FormatFunc
}

func WithModulePath(path string) Option {
	return func(c *config) {
		c.modulePath = path
	}
}

func newConfig(opts []Option) *config {
	c := &config{
		formatFunc: defaultFormatFunc,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func defaultFormatFunc(ctx *Context, is []*Import) GroupedImports {
	groupOfImport := func(pkg string) int {
		if ctx.ModulePath != "" && strings.HasPrefix(pkg, ctx.ModulePath) {
			return 2
		}
		if strings.Contains(pkg, ".") {
			return 1
		}
		return 0
	}

	gi := make(GroupedImports)
	for _, i := range is {
		gi.Add(groupOfImport(i.Path), i)
	}

	for _, is := range gi {
		sort.Slice(is, func(i, j int) bool {
			return is[i].Path < is[j].Path
		})
	}

	return gi
}

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

func loadImports(f *ast.File) ([]*Import, error) {
	var imports []*Import
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		if gd.Tok != token.IMPORT {
			continue
		}

		var docs []string
		if gd.Doc != nil {
			for _, c := range gd.Doc.List {
				docs = append(docs, strings.TrimSpace(strings.TrimPrefix(c.Text, "//")))
			}
		}

		var isNotFirst bool
		for _, s := range gd.Specs {
			if isNotFirst {
				docs = nil
			}
			isNotFirst = true
			is := s.(*ast.ImportSpec)

			path, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				path = is.Path.Value
			}
			impt := &Import{
				Path: path,
				Docs: docs,
			}
			if is.Doc != nil {
				for _, c := range is.Doc.List {
					impt.Docs = append(impt.Docs, strings.TrimSpace(strings.TrimPrefix(c.Text, "//")))
				}
			}
			if is.Comment != nil {
				for _, c := range is.Comment.List {
					// Not sure if having more than 1 comment.
					impt.Comment += strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
				}
			}
			if is.Name != nil {
				impt.Name = is.Name.Name
			}
			imports = append(imports, impt)
		}
	}

	return imports, nil
}

type Import struct {
	Name    string
	Path    string
	Docs    []string
	Comment string
}

type GroupedImports map[int][]*Import

func (gp GroupedImports) Add(n int, i *Import) {
	gp[n] = append(gp[n], i)
}

func (gp GroupedImports) WriteTo(w io.Writer) (written int64, _ error) {
	if len(gp) == 0 {
		return 0, nil
	}

	gs := make([]int, len(gp))
	var i int
	for g := range gp {
		gs[i] = g
		i++
	}

	sort.Ints(gs)

	n, err := fmt.Fprintln(w, "import (")
	if err != nil {
		return 0, err
	}
	written += int64(n)

	for i, g := range gs {
		is := gp[g]

		if i > 0 {
			n, err = fmt.Fprintln(w)
			if err != nil {
				return 0, err
			}
			written += int64(n)
		}

		for _, i := range is {
			for _, d := range i.Docs {
				n, err := fmt.Fprintf(w, "\t// %s\n", d)
				if err != nil {
					return 0, err
				}
				written += int64(n)
			}

			n, err := fmt.Fprint(w, "\t")
			if err != nil {
				return 0, err
			}
			written += int64(n)

			if i.Name != "" {
				n, err := fmt.Fprintf(w, "%s ", i.Name)
				if err != nil {
					return 0, err
				}
				written += int64(n)
			}

			n, err = fmt.Fprintf(w, `"%s"`, i.Path)
			if err != nil {
				return 0, err
			}
			written += int64(n)

			if i.Comment != "" {
				n, err = fmt.Fprintf(w, " // %s", i.Comment)
				if err != nil {
					return 0, err
				}
				written += int64(n)
			}

			n, err = fmt.Fprintln(w)
			if err != nil {
				return 0, err
			}
			written += int64(n)
		}
	}

	n, err = fmt.Fprintln(w, ")")
	if err != nil {
		return 0, err
	}
	written += int64(n)

	return written, nil
}

type FormatFunc func(*Context, []*Import) GroupedImports

type Context struct {
	ModulePath string
}

func removeImports(fs *token.FileSet, f *ast.File) (firstImportLine int, _ error) {
	for i := 0; i < len(f.Decls); i++ {
		d := f.Decls[i]

		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}

		if gd.Tok != token.IMPORT {
			continue
		}

		if firstImportLine == 0 {
			firstImportLine = fs.Position(gd.Pos()).Line
		}

		var comments []*ast.CommentGroup
		if gd.Doc != nil {
			comments = append(comments, gd.Doc)
		}

		for _, s := range gd.Specs {
			is := s.(*ast.ImportSpec)
			if is.Doc != nil {
				comments = append(comments, is.Doc)
			}
			if is.Comment != nil {
				comments = append(comments, is.Comment)
			}
		}

		for _, c1 := range comments {
			for j := 0; j < len(f.Comments); j++ {
				c2 := f.Comments[j]
				if c1 == c2 {
					f.Comments = append(f.Comments[:j], f.Comments[j+1:]...)
					j--
				}
			}
		}

		f.Decls = append(f.Decls[:i], f.Decls[i+1:]...)
		i--
	}

	return firstImportLine, nil
}
