// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
)

const (
	tabWidth    = 8
	printerMode = printer.UseSpaces | printer.TabIndent
)

var (
	exitCode = 0
	fileSet  = token.NewFileSet()
)

var (
	flagApply      = flag.Bool("apply", false, "apply edits to source files")
	flagCPUProfile = flag.String("cpuprofile", "", "write CPU profile to file")
	flagVerbose    = flag.Bool("v", false, "verbose output")
	write          = flag.Bool("w", false, "write result to (source) file instead of stdout")
	diff           = flag.Bool("d", false, "display diffs instead of rewriting files")
	flagTests      = flag.Bool("tests", true, "include test source files")
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: stanzas [flags] [package ...]\n")
	flag.PrintDefaults()
}

func main() {
	stanzasMain()
	os.Exit(exitCode)
}

func stanzasMain() {
	flag.Usage = usage
	flag.Parse()

	if *flagCPUProfile != "" {
		f, err := os.Create(*flagCPUProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "creating cpu profile: %s\n", err)
			exitCode = 2
			return
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if flag.NArg() == 0 {
		if *write {
			fmt.Fprintln(os.Stderr, "error: cannot use -w with standard input")
			exitCode = 2
			return
		}
		if err := processFile("<standard input>", os.Stdin, os.Stdout, true); err != nil {
			report(err)
		}
		return
	}

	for i := 0; i < flag.NArg(); i++ {
		path := flag.Arg(i)
		switch dir, err := os.Stat(path); {
		case err != nil:
			report(err)
		case dir.IsDir():
			walkDir(path)
		default:
			if err := processFile(path, nil, os.Stdout, false); err != nil {
				report(err)
			}
		}
	}
}

// If in == nil, the source is the contents of the file with the given filename.
func processFile(filename string, in io.Reader, out io.Writer, stdin bool) error {
	var perm os.FileMode = 0644
	if in == nil {
		f, err := os.Open(filename)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		in = f
		perm = fi.Mode().Perm()
	}

	src, err := ioutil.ReadAll(in)
	if err != nil {
		return err
	}

	file, err := parser.ParseFile(fileSet, filename, src, parser.ParseComments)
	if err != nil {
		return err
	}

	ast.SortImports(fileSet, file)
	imps := imports(fileSet, file)
	var spacesBefore []string // import paths we need spaces before
	for _, impSection := range imps {
		// Within each block of contiguous imports, see if any
		// import lines are in different group numbers. If so,
		// we'll need to put a space between them so it's
		// compatible with gofmt.
		lastGroup := -1
		for _, importSpec := range impSection {
			importPath, _ := strconv.Unquote(importSpec.Path.Value)
			groupNum := importGroup(importPath)
			if groupNum != lastGroup && lastGroup != -1 {
				spacesBefore = append(spacesBefore, importPath)
			}
			lastGroup = groupNum
		}
	}

	cfg := printer.Config{
		Mode:     printerMode,
		Tabwidth: tabWidth,
	}
	var buf bytes.Buffer
	if err := cfg.Fprint(&buf, fileSet, file); err != nil {
		return err
	}
	res := buf.Bytes()
	if len(spacesBefore) > 0 {
		res, err = addImportSpaces(bytes.NewReader(res), spacesBefore)
		if err != nil {
			return err
		}
	}

	if !bytes.Equal(src, res) {
		if *write {
			// make a temporary backup before overwriting original
			bakname, err := backupFile(filename+".", src, perm)
			if err != nil {
				return err
			}
			err = ioutil.WriteFile(filename, res, perm)
			if err != nil {
				os.Rename(bakname, filename)
				return err
			}
			err = os.Remove(bakname)
			if err != nil {
				return err
			}
		}
		if *diff {
			data, err := diffFile(src, res, filename)
			if err != nil {
				return fmt.Errorf("computing diff: %s", err)
			}
			fmt.Printf("diff -u %s %s\n", filepath.ToSlash(filename+".orig"), filepath.ToSlash(filename))
			out.Write(data)
		}
	}

	if !*write && !*diff {
		_, err = out.Write(res)
	}

	return err
}

// Imports returns the file imports grouped by paragraph.
func imports(fset *token.FileSet, f *ast.File) [][]*ast.ImportSpec {
	var groups [][]*ast.ImportSpec

	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.IMPORT {
			break
		}

		group := []*ast.ImportSpec{}

		var lastLine int
		for _, spec := range genDecl.Specs {
			importSpec := spec.(*ast.ImportSpec)
			pos := importSpec.Path.ValuePos
			line := fset.Position(pos).Line
			if lastLine > 0 && pos > 0 && line-lastLine > 1 {
				groups = append(groups, group)
				group = []*ast.ImportSpec{}
			}
			group = append(group, importSpec)
			lastLine = line
		}
		groups = append(groups, group)
	}

	return groups
}

// LocalPrefix is a comma-separated string of import path prefixes, which, if
// set, instructs Process to sort the import paths with the given prefixes
// into another group after 3rd-party packages.
var LocalPrefix string

func localPrefixes() []string {
	if LocalPrefix != "" {
		return strings.Split(LocalPrefix, ",")
	}
	return nil
}

// importToGroup is a list of functions which map from an import path to
// a group number.
var importToGroup = []func(importPath string) (num int, ok bool){
	func(importPath string) (num int, ok bool) {
		for _, p := range localPrefixes() {
			if strings.HasPrefix(importPath, p) || strings.TrimSuffix(p, "/") == importPath {
				return 3, true
			}
		}
		return
	},
	func(importPath string) (num int, ok bool) {
		if strings.HasPrefix(importPath, "github.com/juju/juju") {
			return 2, true
		}
		return
	},
	func(importPath string) (num int, ok bool) {
		if strings.Contains(importPath, ".") {
			return 1, true
		}
		return
	},
}

func importGroup(importPath string) int {
	for _, fn := range importToGroup {
		if n, ok := fn(importPath); ok {
			return n
		}
	}
	return 0
}

func report(err error) {
	scanner.PrintError(os.Stderr, err)
	exitCode = 2
}

func isGoFile(f os.FileInfo) bool {
	// ignore non-Go files
	name := f.Name()
	return !f.IsDir() && !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".go")
}

func visitFile(path string, f os.FileInfo, err error) error {
	if err == nil && isGoFile(f) {
		err = processFile(path, nil, os.Stdout, false)
	}
	// Don't complain if a file was deleted in the meantime (i.e.
	// the directory changed concurrently while running gofmt).
	if err != nil && !os.IsNotExist(err) {
		report(err)
	}
	return nil
}

func walkDir(path string) {
	filepath.Walk(path, visitFile)
}

const chmodSupported = runtime.GOOS != "windows"

func writeTempFile(dir, prefix string, data []byte) (string, error) {
	file, err := ioutil.TempFile(dir, prefix)
	if err != nil {
		return "", err
	}
	_, err = file.Write(data)
	if err1 := file.Close(); err == nil {
		err = err1
	}
	if err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

func diffFile(b1, b2 []byte, filename string) (data []byte, err error) {
	f1, err := writeTempFile("", "gofmt", b1)
	if err != nil {
		return
	}
	defer os.Remove(f1)

	f2, err := writeTempFile("", "gofmt", b2)
	if err != nil {
		return
	}
	defer os.Remove(f2)

	cmd := "diff"
	if runtime.GOOS == "plan9" {
		cmd = "/bin/ape/diff"
	}

	data, err = exec.Command(cmd, "-u", f1, f2).CombinedOutput()
	if len(data) > 0 {
		// diff exits with a non-zero status when the files don't match.
		// Ignore that failure as long as we get output.
		return replaceTempFilename(data, filename)
	}
	return
}

// replaceTempFilename replaces temporary filenames in diff with actual one.
//
// --- /tmp/gofmt316145376	2017-02-03 19:13:00.280468375 -0500
// +++ /tmp/gofmt617882815	2017-02-03 19:13:00.280468375 -0500
// ...
// ->
// --- path/to/file.go.orig	2017-02-03 19:13:00.280468375 -0500
// +++ path/to/file.go	2017-02-03 19:13:00.280468375 -0500
// ...
func replaceTempFilename(diff []byte, filename string) ([]byte, error) {
	bs := bytes.SplitN(diff, []byte{'\n'}, 3)
	if len(bs) < 3 {
		return nil, fmt.Errorf("got unexpected diff for %s", filename)
	}
	// Preserve timestamps.
	var t0, t1 []byte
	if i := bytes.LastIndexByte(bs[0], '\t'); i != -1 {
		t0 = bs[0][i:]
	}
	if i := bytes.LastIndexByte(bs[1], '\t'); i != -1 {
		t1 = bs[1][i:]
	}
	// Always print filepath with slash separator.
	f := filepath.ToSlash(filename)
	bs[0] = []byte(fmt.Sprintf("--- %s%s", f+".orig", t0))
	bs[1] = []byte(fmt.Sprintf("+++ %s%s", f, t1))
	return bytes.Join(bs, []byte{'\n'}), nil
}

// backupFile writes data to a new file named filename<number> with permissions perm,
// with <number randomly chosen such that the file name is unique. backupFile returns
// the chosen file name.
func backupFile(filename string, data []byte, perm os.FileMode) (string, error) {
	// create backup file
	f, err := ioutil.TempFile(filepath.Dir(filename), filepath.Base(filename))
	if err != nil {
		return "", err
	}
	bakname := f.Name()
	if chmodSupported {
		err = f.Chmod(perm)
		if err != nil {
			f.Close()
			os.Remove(bakname)
			return bakname, err
		}
	}

	// write data to backup file
	_, err = f.Write(data)
	if err1 := f.Close(); err == nil {
		err = err1
	}

	return bakname, err
}

var impLine = regexp.MustCompile(`^\s+(?:[\w\.]+\s+)?"(.+)"`)

func addImportSpaces(r io.Reader, breaks []string) ([]byte, error) {
	var out bytes.Buffer
	in := bufio.NewReader(r)
	inImports := false
	done := false
	for {
		s, err := in.ReadString('\n')
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		if !inImports && !done && strings.HasPrefix(s, "import") {
			inImports = true
		}
		if inImports && (strings.HasPrefix(s, "var") ||
			strings.HasPrefix(s, "func") ||
			strings.HasPrefix(s, "const") ||
			strings.HasPrefix(s, "type")) {
			done = true
			inImports = false
		}
		if inImports && len(breaks) > 0 {
			if m := impLine.FindStringSubmatch(s); m != nil {
				if m[1] == breaks[0] {
					out.WriteByte('\n')
					breaks = breaks[1:]
				}
			}
		}

		fmt.Fprint(&out, s)
	}
	return out.Bytes(), nil
}
