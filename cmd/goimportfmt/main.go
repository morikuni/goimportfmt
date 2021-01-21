package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/morikuni/failure"
	"github.com/morikuni/go-appmain"
	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

func main() {
	os.Exit(createApp().Run())
}

func createApp() *appmain.App {
	var verbose bool
	app := appmain.New(
		appmain.ErrorStrategy(func(tc appmain.TaskContext) appmain.Decision {
			err := tc.Err()
			if err != nil {
				message, ok := failure.MessageOf(err)
				if !ok {
					message = err.Error()
				}
				fmt.Fprintln(os.Stderr, message)

				if verbose {
					fmt.Fprintf(os.Stderr, "%+v\n", err)
				}
			}
			return appmain.DefaultErrorStrategy(tc)
		}),
	)

	var modulePath string
	var target string

	var writeBack bool
	app.AddInitTask("process flags", func(ctx context.Context) error {
		fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		module := fs.String("module", "", "specify the module path")
		fs.BoolVar(&verbose, "v", false, "verbose error format")
		fs.BoolVar(&writeBack, "w", false, "write result to source file")

		err := fs.Parse(os.Args[1:])
		if err != nil {
			return failure.Wrap(err, failure.Message("failed to parse flags"))
		}

		if len(fs.Args()) < 1 {
			return failure.Unexpected("require filename", failure.Message("filename required"))
		}

		target = fs.Arg(0)

		fi, err := os.Stat(target)
		if err != nil && os.IsNotExist(err) {
			return failure.Wrap(err, failure.Messagef("file not found: %s", target))
		}

		modulePath = *module
		if modulePath != "" {
			return nil
		}

		if fi.IsDir() {
			return failure.Unexpected("directory", failure.Message("directory is not supported"))
		}

		dir, file := filepath.Split(target)
		dir, err = filepath.Abs(dir)
		if err != nil {
			return failure.Wrap(err, failure.Messagef("invalid path: %s", target))
		}

		cfg := &packages.Config{
			Dir:  dir,
			Mode: packages.NeedModule,
		}
		pkgs, err := packages.Load(cfg, file)
		if err != nil {
			return failure.Wrap(err, failure.Messagef("failed to load the package of %q: %v", target, err))
		}

		var pkgErr error
		packages.Visit(pkgs, nil, func(p *packages.Package) {
			for _, e := range p.Errors {
				if strings.Contains(e.Msg, "outside available modules") {
					continue
				}
				if strings.Contains(e.Msg, "working directory is not part of a module") {
					continue
				}
				pkgErr = failure.Unexpected(e.Msg, failure.Message("failed to load package: "+e.Msg))
			}
		})
		if pkgErr != nil {
			return failure.Wrap(pkgErr)
		}

		if len(pkgs) == 0 {
			return nil
		}
		if len(pkgs) > 1 {
			return failure.Unexpected("found 2 or more package",
				failure.Message("unexpected error during loading package"),
				failure.Context{"len": strconv.Itoa(len(pkgs))},
			)
		}

		pkg := pkgs[0]

		if pkg.Module == nil {
			return nil
		}

		modFile := pkg.Module.GoMod
		if modFile == "" {
			return nil
		}

		bs, err := ioutil.ReadFile(modFile)
		if err != nil {
			return failure.Wrap(err, failure.Messagef("failed to load %s: %v", modFile, err))
		}

		modulePath = modfile.ModulePath(bs)
		return nil
	})

	type Import struct {
		Name     string
		Path     string
		Docs     []string
		Comment  string
		LineFrom int
		LineTo   int
	}
	var imports []*Import
	load := app.AddMainTask("load imports", func(ctx context.Context) error {
		fileSet := token.NewFileSet()
		f, err := parser.ParseFile(fileSet, target, nil, parser.ParseComments)
		if err != nil {
			return failure.Wrap(err, failure.Message(err.Error()))
		}

		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			if gd.Tok != token.IMPORT {
				continue
			}

			var docs []string
			var docLine int
			if gd.Doc != nil {
				for _, c := range gd.Doc.List {
					docs = append(docs, c.Text)
				}
				docLine = fileSet.Position(gd.Doc.Pos()).Line
			}

			var isNotFirst bool
			for _, s := range gd.Specs {
				if isNotFirst {
					docs = nil
					docLine = 0
				}
				isNotFirst = true
				is := s.(*ast.ImportSpec)

				pathPos := fileSet.Position(is.Path.Pos())
				path, err := strconv.Unquote(is.Path.Value)
				if err != nil {
					path = is.Path.Value
				}
				impt := &Import{
					Path:     path,
					LineFrom: pathPos.Line,
					LineTo:   pathPos.Line,
					Docs:     docs,
				}
				if docLine != 0 {
					impt.LineFrom = docLine
				}
				if is.Doc != nil {
					impt.LineFrom = fileSet.Position(is.Doc.Pos()).Line
					for _, c := range is.Doc.List {
						impt.Docs = append(impt.Docs, c.Text)
					}
				}
				if is.Comment != nil {
					for _, c := range is.Comment.List {
						// Not sure if having more than 1 comment.
						impt.Comment += c.Text
					}
				}
				if is.Name != nil {
					impt.Name = is.Name.Name
				}
				imports = append(imports, impt)
			}
		}

		return nil
	})

	var result []byte
	sort := app.AddMainTask("sort imports", func(ctx context.Context) error {
		if load.Err() != nil {
			return nil
		}

		if len(imports) == 0 {
			return nil
		}
		lastImportLine := imports[len(imports)-1].LineTo
		removeLines := make(map[int]struct{}, len(imports)*2)
		for _, imp := range imports {
			for i := imp.LineFrom; i <= imp.LineTo; i++ {
				removeLines[i] = struct{}{}
			}
		}

		f, err := os.Open(target)
		if err != nil {
			return failure.Wrap(err, failure.Messagef("failed to open file: %s", target))
		}
		defer f.Close()

		buf := &bytes.Buffer{}
		sc := bufio.NewScanner(f)
		var afterFirstImport bool
		var inImport bool
		var lineNum int
		var linesInImport []string
		var nonImportLines []string
	LOOP:
		for sc.Scan() {
			lineNum++
			line := sc.Text()

			_, removeLine := removeLines[lineNum]
			switch {
			case lineNum > lastImportLine && !inImport:
				break LOOP
			case strings.HasPrefix(line, "import"):
				afterFirstImport = true
				if strings.Contains(line, "(") {
					inImport = true
				}
			case !afterFirstImport:
				fmt.Fprintln(buf, line)
			case inImport && strings.TrimSpace(line) == ")":
				inImport = false
			case removeLine:
			case !inImport:
				nonImportLines = append(nonImportLines, line)
			default:
				// inImport
				if line != "" {
					linesInImport = append(linesInImport, line)
				}
			}
		}
		if sc.Err() != nil {
			return failure.Wrap(sc.Err(), failure.Messagef("failed to read file: %s", target))
		}

		fmt.Fprintln(buf, "import (")
		for _, l := range linesInImport {
			fmt.Fprintln(buf, l)
		}

		fmt.Fprintln(buf)

		sort.Slice(imports, func(i, j int) bool {
			return imports[i].Path < imports[j].Path
		})

		// print doc imports first
		var docImports []*Import
		var nonDocImports []*Import
		for _, i := range imports {
			if len(i.Docs) == 0 {
				nonDocImports = append(nonDocImports, i)
			} else {
				docImports = append(docImports, i)
			}
		}

		groups := make(map[int][]*Import)
		for _, i := range nonDocImports {
			g := groupOfImport(i.Path, modulePath)
			groups[g] = append(groups[g], i)
		}
		var gs []int
		for g := range groups {
			gs = append(gs, g)
		}
		sort.Slice(gs, func(i, j int) bool {
			return gs[i] < gs[j]
		})
		blocks := make([][]*Import, 0, len(gs))
		for _, g := range gs {
			blocks = append(blocks, groups[g])
		}

		printImports := func(is []*Import) {
			for _, i := range is {
				for _, d := range i.Docs {
					fmt.Fprintln(buf, d)
				}
				fmt.Fprintf(buf, `%s "%s"`, i.Name, i.Path)
				if i.Comment != "" {
					fmt.Fprintf(buf, " %s\n", i.Comment)
				} else {
					fmt.Fprintln(buf)
				}
			}
		}

		printImports(docImports)
		fmt.Fprintln(buf)
		for _, b := range blocks {
			printImports(b)
			fmt.Fprintln(buf)
		}

		fmt.Fprintln(buf, ")")

		for _, l := range nonImportLines {
			fmt.Fprintln(buf, l)
		}

		for sc.Scan() {
			fmt.Fprintln(buf, sc.Text())
		}
		if sc.Err() != nil {
			return failure.Wrap(sc.Err(), failure.Messagef("failed to read file: %s", target))
		}

		result, err = format.Source(buf.Bytes())
		if err != nil {
			return failure.Wrap(err, failure.Message("failed to format source"))
		}

		return nil
	}, appmain.RunAfter(load))

	app.AddMainTask("write result", func(ctx context.Context) error {
		if sort.Err() != nil {
			return nil
		}

		if len(result) == 0 {
			return nil
		}

		var out io.Writer = os.Stdout
		if writeBack {
			f, err := os.Create(target)
			if err != nil {
				return failure.Wrap(err, failure.Messagef("failed to open file: %s", target))
			}
			defer f.Close()
			out = f
		}

		_, err := out.Write(result)
		if err != nil {
			return failure.Wrap(err, failure.Messagef("failed to write to file: %s", target))
		}

		return nil
	}, appmain.RunAfter(sort))

	return app
}

func groupOfImport(pkg string, mod string) int {
	if !strings.Contains(pkg, ".") {
		return 0
	}
	if mod != "" && strings.HasPrefix(pkg, mod) {
		return 2
	}
	return 1
}
