// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// godoc2md converts godoc formatted package documentation into Markdown format.
//
//
// Usage
//
//    godoc2md $PACKAGE > $GOPATH/src/$PACKAGE/README.md
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/build"
	"go/printer"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/godoc"
	"golang.org/x/tools/godoc/vfs"
)

var (
	verbose = flag.Bool("v", false, "verbose mode")

	// file system roots
	// TODO(gri) consider the invariant that goroot always end in '/'
	goroot = flag.String("goroot", runtime.GOROOT(), "Go root directory")

	// layout control
	tabWidth       = flag.Int("tabwidth", 4, "tab width")
	showTimestamps = flag.Bool("timestamps", false, "show timestamps with directory listings")
	altPkgTemplate = flag.String("template", "", "path to an alternate template file")
	showPlayground = flag.Bool("play", false, "enable playground in web interface")
	showExamples   = flag.Bool("ex", false, "show examples in command line mode")
	declLinks      = flag.Bool("links", true, "link identifiers to their declarations")
	outFile        = flag.String("o", "", "output file path. Writes to stdout if unspecified or equal to -")

	// The hash format for Github is the default `#L%d`; but other source control platforms do not
	// use the same format. For example Bitbucket Enterprise uses `#%d`. This option provides the
	// user the option to switch the format as needed and still remain backwards compatible.
	srcLinkHashFormat = flag.String("hashformat", "#L%d", "source link URL hash format")
	srcLinkFormat     = flag.String("srclink", "", "if set, format for entire source link")

	// Patterns used to rewrite the package names to http urls for github and
	// bitbucket and the suffix to place between the root of the repo and the
	// rest. Those come from https://github.com/golang/gddo/tree/master/gosrc
	gitPatterns = []struct {
		pattern *regexp.Regexp
		suffix  string
	}{
		// github.com
		{regexp.MustCompile(`^(github\.com)/(?P<owner>[a-z0-9A-Z_.\-]+)/(?P<repo>[a-z0-9A-Z_.\-]+)(?P<dir>/.*)?$`), "tree/master"},
		// bitbucket.com
		{regexp.MustCompile(`^(bitbucket\.org)/(?P<owner>[a-z0-9A-Z_.\-]+)/(?P<repo>[a-z0-9A-Z_.\-]+)(?P<dir>/[a-z0-9A-Z_.\-/]*)?$`), "src/master"},
		// all other
		{regexp.MustCompile(`^(?P<domain>[a-z0-9A-Z_.\-]+\.[a-z]+)/(?P<owner>[a-z0-9A-Z_.\-]+)/(?P<repo>[a-z0-9A-Z_.\-]+)(?P<dir>/[a-z0-9A-Z_.\-/]*)?$`), "src"},
	}
)

func usage() {
	fmt.Fprintf(os.Stderr,
		"usage: godoc2md package [name ...]\n")
	flag.PrintDefaults()
	os.Exit(2)
}

var (
	pres *godoc.Presentation
	fs   = vfs.NameSpace{}

	funcs = map[string]interface{}{
		"example_md":  exampleMdFunc,
		"comment_md":  commentMdFunc,
		"base":        path.Base,
		"md":          mdFunc,
		"pre":         preFunc,
		"kebab":       kebabFunc,
		"bitscape":    bitscapeFunc, //Escape [] for bitbucket confusion
		"trim_prefix": strings.TrimPrefix,
		"clean_link":  cleanLink,
	}
)

func cleanLink(src string) string {
	src = strings.ToLower(src)
	return strings.Replace(src, "_", "", -1)
}

// Comes from https://github.com/golang/tools/blob/master/godoc/godoc.go#L786
func stripExampleSuffix(name string) string {
	if i := strings.LastIndex(name, "_"); i != -1 {
		if i < len(name)-1 && !startsWithUppercase(name[i+1:]) {
			name = name[:i]
		}
	}
	return name
}

// Comes from https://github.com/golang/tools/blob/master/godoc/godoc.go#L777
func startsWithUppercase(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

// Based on example_textFunc from
// https://github.com/golang/tools/blob/master/godoc/godoc.go
func exampleMdFunc(info *godoc.PageInfo, funcName string) string {
	if !*showExamples {
		return ""
	}

	var buf bytes.Buffer
	first := true
	for _, eg := range info.Examples {
		name := stripExampleSuffix(eg.Name)
		if name != funcName {
			continue
		}

		if !first {
			buf.WriteString("\n")
		}
		first = false

		// print code
		cnode := &printer.CommentedNode{Node: eg.Code, Comments: eg.Comments}
		config := &printer.Config{Mode: printer.UseSpaces, Tabwidth: pres.TabWidth}
		var buf1 bytes.Buffer
		config.Fprint(&buf1, info.FSet, cnode)
		code := buf1.String()

		// Additional formatting if this is a function body. Unfortunately, we
		// can't print statements individually because we would lose comments
		// on later statements.
		if n := len(code); n >= 2 && code[0] == '{' && code[n-1] == '}' {
			// remove surrounding braces
			code = code[1 : n-1]
		}
		code = strings.Trim(code, "\n")
		title := fmt.Sprintf("##### Example %s:\n", strings.Replace(funcName, "_", ".", -1))
		buf.WriteString(title)
		buf.WriteString("``` go\n")
		buf.WriteString(code)
		buf.WriteString("\n```\n\n")
	}

	if buf.Len() == 0 {
		log.Printf("warning: empty buffer")
	}

	return buf.String()
}

func commentMdFunc(comment string) string {
	var buf bytes.Buffer
	ToMD(&buf, comment)
	return buf.String()
}

func mdFunc(text string) string {
	text = strings.Replace(text, "*", "\\*", -1)
	text = strings.Replace(text, "_", "\\_", -1)
	return text
}

func preFunc(text string) string {
	return "``` go\n" + text + "\n```"
}

// Original Source https://github.com/golang/tools/blob/master/godoc/godoc.go#L562
func srcLinkFunc(s string) string {
	s = path.Clean("/" + s)
	return strings.TrimPrefix(s, "/target")
}

// Removed code line that always subtracted 10 from the value of `line`.
// Made format for the source link hash configurable to support source control platforms other than Github.
// Original Source https://github.com/golang/tools/blob/master/godoc/godoc.go#L540
func srcPosLinkFunc(s string, line, low, high int) string {
	if *srcLinkFormat != "" {
		return fmt.Sprintf(*srcLinkFormat, s, line, low, high)
	}

	s = srcLinkFunc(s)
	var buf bytes.Buffer
	template.HTMLEscape(&buf, []byte(s))
	// selection ranges are of form "s=low:high"
	if low < high {
		fmt.Fprintf(&buf, "?s=%d:%d", low, high) // no need for URL escaping
		if line < 1 {
			line = 1
		}
	}
	// line id's in html-printed source are of the
	// form "L%d" (on Github) where %d stands for the line number
	if line > 0 {
		fmt.Fprintf(&buf, *srcLinkHashFormat, line) // no need for URL escaping
	}
	return buf.String()
}

func readTemplate(name, data string) *template.Template {
	// be explicit with errors (for app engine use)
	t, err := template.New(name).Funcs(pres.FuncMap()).Funcs(funcs).Parse(data)
	if err != nil {
		log.Fatal("readTemplate: ", err)
	}
	return t
}

func kebabFunc(text string) string {
	s := strings.Replace(strings.ToLower(text), " ", "-", -1)
	s = strings.Replace(s, ".", "-", -1)
	s = strings.Replace(s, "\\*", "42", -1)
	return s
}

func bitscapeFunc(text string) string {
	s := strings.Replace(text, "[", "\\[", -1)
	s = strings.Replace(s, "]", "\\]", -1)
	return s
}

// rewriteURL is used to rewrite urls from a github package source file
func rewriteURL(src, suffix string, pattern *regexp.Regexp) string {
	result := ""
	if m := pattern.FindStringSubmatch(src); m != nil {
		result = fmt.Sprintf("https://%s/%s/%s/%s", m[1], m[2], m[3], suffix)
		if m[4] != "" {
			result = fmt.Sprintf("%s%s", result, m[4])
		}
	}
	return result
}

// Rewriting a source file path to its http equivalent and making sure you can
// add a file a file path after without having to worry about the element that
// comes between the root of the repository and the repo path
func urlFromPackage(src string) string {
	// the source for golang.org/x is on github
	src = strings.Replace(src, "golang.org/x", "github.com/golang", -1)
	// other packages
	for _, pat := range gitPatterns {
		if pat.pattern.MatchString(src) {
			return rewriteURL(src, pat.suffix, pat.pattern)
		}
	}
	return fmt.Sprintf("https://golang.org/src/%s", src)
}

func main() {
	flag.Usage = usage
	flag.Parse()

	// Check usage
	if flag.NArg() == 0 {
		usage()
	}

	// use file system of underlying OS
	fs.Bind("/", vfs.OS(*goroot), "/", vfs.BindReplace)

	// Bind $GOPATH trees into Go root.
	for _, p := range filepath.SplitList(build.Default.GOPATH) {
		fs.Bind("/src/pkg", vfs.OS(p), "/src", vfs.BindAfter)
	}

	corpus := godoc.NewCorpus(fs)
	corpus.Verbose = *verbose

	pres = godoc.NewPresentation(corpus)
	pres.TabWidth = *tabWidth
	pres.ShowTimestamps = *showTimestamps
	pres.ShowPlayground = *showPlayground
	pres.ShowExamples = *showExamples
	pres.DeclLinks = *declLinks
	pres.SrcMode = false
	pres.HTMLMode = false
	pres.URLForSrcPos = srcPosLinkFunc
	pres.URLForSrc = urlFromPackage

	if *altPkgTemplate != "" {
		buf, err := ioutil.ReadFile(*altPkgTemplate)
		if err != nil {
			log.Fatal(err)
		}
		pres.PackageText = readTemplate("package.txt", string(buf))
	} else {
		pres.PackageText = readTemplate("package.txt", pkgTemplate)
	}

	of := os.Stdout
	if *outFile != "" && *outFile != "-" {
		var err error
		of, err = os.Create(*outFile)
		if err != nil {
			log.Fatal(err)
		}
		defer of.Close()
	}

	if err := godoc.CommandLine(of, fs, pres, flag.Args()); err != nil {
		log.Print(err)
	}
}
