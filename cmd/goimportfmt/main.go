package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/morikuni/goimportfmt"
)

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	write := fs.Bool("w", false, "write result to source file.")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] filename\n", os.Args[0])
		fs.PrintDefaults()
	}

	err := fs.Parse(os.Args[1:])
	if err != nil {
		panic(err)
	}

	filename := fs.Arg(0)
	src, err := os.Open(filename)
	if err != nil {
		panic(err)
	}
	var once sync.Once
	close := func() {
		once.Do(func() {
			src.Close()
		})
	}
	defer close()

	p, err := goimportfmt.DetectModulePath(filename)
	if err != nil {
		panic(err)
	}

	stat, err := src.Stat()
	if err != nil {
		panic(err)
	}

	buf := bytes.NewBuffer(make([]byte, 0, int(stat.Size())))
	err = goimportfmt.Process(src, buf, goimportfmt.WithModulePath(p))
	if err != nil {
		panic(err)
	}

	if *write {
		close()
		f, err := os.Create(filename)
		if err != nil {
			panic(err)
		}

		_, err = io.Copy(f, buf)
		if err != nil {
			panic(err)
		}
	} else {
		_, err = io.Copy(os.Stdout, buf)
		if err != nil {
			panic(err)
		}
	}
}
