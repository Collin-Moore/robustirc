// +build ignore

package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io"
	"io/ioutil"
	"log"
	"strings"
	"text/template"
	"unicode/utf8"
)

var output = flag.String("output", "templates.go", "Where to put the variable declarations")
var pkgname = flag.String("package", "main", "What package should the output file belong to")

// errWriter wraps an io.Writer and saves the first error it encounters (and
// otherwise ignores all write-errors).
type errWriter struct {
	io.Writer
	err error
}

// Like strconv.CanBackquote, but allows multi-line backquoted strings (i.e.
// does not consider \n in the input a cause to return false).
func canBackquote(s string) bool {
	for len(s) > 0 {
		r, wid := utf8.DecodeRuneInString(s)
		s = s[wid:]
		if wid > 1 {
			if r == '\ufeff' {
				return false // BOMs are invisible and should not be quoted.
			}
			continue // All other multibyte runes are correctly encoded and assumed printable.
		}
		if r == utf8.RuneError {
			return false
		}
		if (r < ' ' && r != '\t' && r != '\n') || r == '`' || r == '\u007F' {
			return false
		}
	}
	return true
}

var embeddedTemplateTmpl = template.Must(template.New("embedded").Parse(`
package {{ .Package }}

// Generated by "go run gentmpl.go {{ .Args }}".
// Do not edit manually.

import (
	"html/template"
)

var templates = template.New("all")

func init() {
	{{ range $i, $t := .Templates -}}
	template.Must(templates.New("{{ $t.Name }}").Parse({{ $t.ContentLiteral }}))
	{{ end -}}
}
`))

func main() {
	flag.Parse()

	if flag.NArg() < 1 {
		log.Fatal("Must provide at least one template file to include")
	}

	type nameAndContent struct {
		Name           string
		ContentLiteral string
	}
	tmpls := make([]nameAndContent, flag.NArg())
	for i := 0; i < flag.NArg(); i++ {
		n := flag.Arg(i)
		b, err := ioutil.ReadFile(n + ".html")
		if err != nil {
			log.Fatal(err)
		}
		// If we can use backquotes, we do — it makes generated files easier to read.
		var literal string
		if canBackquote(string(b)) {
			literal = "`" + string(b) + "`"
		} else {
			literal = fmt.Sprintf("%#v", string(b))
		}
		tmpls[i] = nameAndContent{
			Name:           flag.Arg(i),
			ContentLiteral: literal,
		}
	}

	var buf bytes.Buffer
	if err := embeddedTemplateTmpl.Execute(&buf, struct {
		Package   string
		Args      string
		Templates []nameAndContent
	}{
		Package:   *pkgname,
		Args:      strings.Join(flag.Args(), " "),
		Templates: tmpls,
	}); err != nil {
		log.Fatal(err)
	}

	outsrc, err := format.Source(buf.Bytes())
	if err != nil {
		log.Fatalf("Could not format output: %v\nOutput:\n%s", buf.String(), err)
	}

	if err := ioutil.WriteFile(*output, outsrc, 0644); err != nil {
		log.Fatalf("Could not write output: %v", err)
	}
}
