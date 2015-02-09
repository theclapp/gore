package eval

/*
 eval provides a single function, Eval, that "evaluates" its argument. See documentation for Eval for more details
 author: Sriram Srinivasan (sriram@malhar.net)
*/

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

var (
	builtinPkgs map[string]string
)

func init() {
	builtinPkgs = make(map[string]string)
	pkgs := []string{
		"hash/adler32", "crypto/aes", "encoding/ascii85", "encoding/asn1",
		"go/ast", "sync/atomic", "encoding/base32", "encoding/base64",
		"math/big", "encoding/binary", "bufio", "go/build",
		"bytes", "compress/bzip2", "net/http/cgi", "runtime/cgo",
		"crypto/cipher", "math/cmplx", "image/color", "hash/crc32",
		"hash/crc64", "crypto", "encoding/csv", "runtime/debug",
		"crypto/des", "go/doc", "image/draw", "database/sql/driver",
		"crypto/dsa", "debug/dwarf", "crypto/ecdsa", "debug/elf",
		"crypto/elliptic", "errors", "os/exec", "expvar",
		"net/http/fcgi", "path/filepath", "flag", "compress/flate",
		"fmt", "hash/fnv", "image/gif", "encoding/gob",
		"debug/gosym", "compress/gzip", "hash", "container/heap",
		"encoding/hex", "crypto/hmac", "html", "net/http",
		"net/http/httputil", "image", "io", "io/ioutil",
		"image/jpeg", "encoding/json", "net/rpc/jsonrpc", "container/list",
		"log", "compress/lzw", "debug/macho", "net/mail",
		"math", "crypto/md5", "mime", "mime/multipart",
		"net", "os", "text/template/parse", "go/parser",
		"path", "debug/pe", "encoding/pem", "crypto/x509/pkix",
		"image/png", "net/http/pprof", "go/printer",
		"math/rand", "crypto/rc4", "reflect",
		"regexp", "container/ring", "net/rpc", "crypto/rsa",
		"runtime", "text/scanner", "crypto/sha1",
		"crypto/sha256", "crypto/sha512", "os/signal", "net/smtp",
		"sort", "database/sql", "strconv", "strings",
		"crypto/subtle", "index/suffixarray", "sync", "regexp/syntax",
		"syscall", "log/syslog", "text/tabwriter", "archive/tar",
		"text/template", "net/textproto", "time",
		"crypto/tls", "go/token", "unicode", "unsafe",
		"net/url", "os/user", "unicode/utf16", "unicode/utf8",
		"crypto/x509", "encoding/xml", "archive/zip", "compress/zlib",
	}

	for _, pkg := range pkgs {
		builtinPkgs[pkg[strings.LastIndex(pkg, "/")+1:]] = pkg
	}
}

// Eval "evaluates" a multi-line bit of go code by compiling and running it. It
// returns either a non-blank compiler error, or the combined stdout and stderr output
// generated by the evaluated code.
// Eval is designed to help interactive exploration, and so provides
// the conveniences illustrated in the example below
//   Eval(`
//         p "Eval demo"
//         type A struct {
//               S string
//               V int
//         }
//         a := A{S: "The answer is", V: 42}
//         p "a = ", a
//         fmt.Printf("%s: %d\n", a.S, a.V)
// `)
// This should return:
//     Eval demo
//     a =  {The answer is 42}
//     The answer is: 42
//
// 1. A line of the form "p XXX" is translated to _p(XXX), where _p is an embedded function (see buildMain)
// 2. There is no need to import standard go packages. They are inferred
//    and imported automatically. (e.g. "fmt" in the code above)
// 3. The code is automatically wrapped inside a main package and a main function.
//    Statements are internally reordered, so that import blocks, type declaration blocks and funcs
//    are pulled to the "top level"; i.e precede the other statements. The remaining statements and blocks
//    are bundled inside a main function.
// To examine the generated code, set the envvar TMPDIR or TEMPDIR, and see $TMPDIR/gore_eval.go

func Eval(code string) (out string, err string) {
	defer func() { // error recovery
		if e := recover(); e != nil {
			out = ""
			err = fmt.Sprintf("1:%v", e)
		}
	}()

	// No additional wrapping if it has a package declaration already
	if ok, _ := regexp.MatchString(`^\s*package `, code); ok {
		out, err = run(code)
		return out, err
	}

	code = expandAliases(code)
	topLevel, nonTopLevel, pkgsToImport := partition(code)
	return buildAndExec(topLevel, nonTopLevel, pkgsToImport)
}

// A Chunk is a stretch of text, and is either a comment or a string (possibly multiline), or text by default

// Chunk kind
const (
	KSTRING = iota + 1
	KCOMMENT
	KTEXT
)

type Chunk struct {
	kind  int    // One of the chunk kinds above
	text  string // slice of input string
	numNL int    // number of new lines embedded in text
}

type State struct {
	// the current line number, while accumulating chunks
	lineNum int
	// inferred set of package names. The map's value is a dummy
	pkgsToImport map[string]bool
	isTopLevel   bool
	// lineNumber where the last bracket was opened
	brackOpenAt int
	// number of parens and curlies that have not been closed
	brackCount int
	// One of ')', '}',  or ' ' as a dummy value.
	closingCh uint8
	// for each line in input code, an array of chunks
	chunks map[int][]Chunk
}

// split code into topLevel and non-topLevel chunks. non-topLevel
// chunks belong inside a main function, and topLevel chunks refer to
// type, func and import blocks.  Because statements and blocks may
// need to be reordered, we embed line numbers of the form "//line
// :nnn" that is understood by the go compiler to refer to the correct
// line number in the original source. This way, errors in the user's
// input are traceable after reordering.
// pkgsToImport contains standard package names inferred from code
//
func partition(code string) (topLevel string, nonTopLevel string, pkgsToImport map[string]bool) {
	state := &State{
		lineNum:      1,
		pkgsToImport: make(map[string]bool),
		isTopLevel:   false,
		brackOpenAt:  0,
		closingCh:    ' ',
		brackCount:   0,
		chunks:       make(map[int][]Chunk),
	}

	topLevel = ""
	nonTopLevel = ""
	scanner := NewScanner(code)
	for {
		chunk, err := nextChunk(scanner)
		if err != nil {
			if err == io.EOF {
				break
			} else {
				panic(err)
			}
		}
		addChunk(state, chunk)
	}

	for lineNum := 1; lineNum <= state.lineNum; lineNum++ {
		line := processLine(lineNum, state)
		if state.isTopLevel {
			topLevel = addLine(lineNum, topLevel, line)
		} else {
			nonTopLevel = addLine(lineNum, nonTopLevel, line)
		}
	}

	if state.brackCount > 0 {
		panic(fmt.Sprintf("%d: Bracket or paren not closed. %d", state.brackOpenAt, state.brackCount))
	}
	return topLevel, nonTopLevel, state.pkgsToImport
}

func addLine(lineNum int, code string, line string) string {
	// add line numbers annotations only if they can be added at beginning of line; that is the earlier bit of code ends in \n
	if len(code) == 0 || code[len(code)-1] == '\n' {
		return code + fmt.Sprintf("//line :%d\n", lineNum) + line
	} else {
		return code + line
	}
	return ""
}

// add a chunk to the current line in state.chunks
func addChunk(state *State, chunk Chunk) {
	chunks, ok := state.chunks[state.lineNum]
	if !ok {
		chunks = []Chunk{}
	}
	state.chunks[state.lineNum] = append(chunks, chunk)
	state.lineNum += chunk.numNL
}

/*func dumpChunks(chunks map[int][]Chunk, maxLine int) {
	for i := 0; i <= maxLine; i++ {
		if chunks, ok := chunks[i]; ok {
			for _, chunk := range chunks {
				print(chunk.text)
			}
		}
	}
}
*/
// Given a line number, extract all the chunks for that line, and
// infer package names, and update numbers of opening and closing parens.
//
// The returned line is the original line prepended by a line number
// compiler pragma, taking care that initial parts of lines may be part of
// a multiline comment or string.
func processLine(lineNum int, state *State) (retLine string) {
	chunks, ok := state.chunks[lineNum]
	if !ok {
		return ""
	}
	for _, chunk := range chunks {
		if chunk.kind == KTEXT {
			inferPackages(chunk.text, state.pkgsToImport)
		}
	}

	// Since import and func declarations are not always on a single line, we need to
	// accumulate whole blocks, which means we have to look for the closing paren (for imports)
	// and curly (for func and type declarations). In order to account for
	// nested parens/curlies, we use a simple (simplistic?) strategy that goes well with
	// go code: only count open parens/brackets at end of line, and closing parens/curlies
	// at beginning of line.

	// To eliminate the presence of curlies and parens inside comments and strings,
	// extract text only from TEXT chunks.

	l := strings.TrimLeft(extractTxt(chunks), " \t")
	if len(l) > 0 {
		// Is there a '}' or ')' at beginning of line, modulo comments
		if l[0] == state.closingCh { //
			state.brackCount--
			if state.brackCount == 0 {
				state.closingCh = ' ' // reset; note l[0] can never be ' ' because of TrimSpace
				state.brackOpenAt = 0
			}
		} else if state.brackCount == 0 {
			// look for func/type/import decls. This is the reason we could not trim trailing spaces
			// earlier
			state.isTopLevel = strings.HasPrefix(l, "func ") ||
				strings.HasPrefix(l, "type ") ||
				strings.HasPrefix(l, "import ")
		}
	}
	l = strings.TrimSpace(l) // trailing whitespace
	if len(l) > 0 {
		// Is there a '{' or '(' at end of line modulo comments
		switch l[len(l)-1] {
		case '{':
			state.closingCh = '}'
			if state.brackCount == 0 {
				state.brackOpenAt = state.lineNum
			}
			state.brackCount++
		case '(':
			state.closingCh = ')'
			if state.brackCount == 0 {
				state.brackOpenAt = state.lineNum
			}
			state.brackCount++
		}
	}

	// Concat chunks' texts
	retLine = ""
	for _, chunk := range chunks {
		retLine += chunk.text
	}
	return retLine
}

// Concatenate chunk.text from TEXT chunks into a single string
func extractTxt(chunks []Chunk) (line string) {
	line = ""
	for _, chunk := range chunks {
		if chunk.kind == KTEXT {
			line += chunk.text
		}
	}
	return line
}

// "p a,b,c" pretty prints each argument; it effectively expands to fmt.Printf("%+v %+v %+v\n", a, b, c)
// "t a,b,c" prints the type of each argument; it effectively expands to fmt.Printf("%T %T %T\n", a, b, c)
// These aliases are expanded only if they are at the beginning of a line, and don't look like
// a method call or variable assignment (e.g. "p := 10", or "p (100)"
func expandAliases(code string) string {
	// Expand "p foo(), 2*3"   to __p(foo(), 2*3). __p is defined in the template in buildMain
	// Look for p followed by spaces followed by something that doesn't start with =, : or (
	r := regexp.MustCompile(`(?m)^\s*p +([^\s=:(].*)$`)
	code = r.ReplaceAllString(code, "__p($1)")

	// Expand "t foo(), 2*3"   to __t(foo(), 2*3), where __t prints the type of each arg
	r = regexp.MustCompile(`(?m)^\s*t +([^\s=:(].*)$`)
	return r.ReplaceAllString(code, "__t($1)")
}

var pkgPat = regexp.MustCompile(`(?m)\b[a-z]\w+\.`)

// Look for strings of the form "xyz.Abc" or "xyz.abc"; we assume "xyz" is an
// imported package, and if the compiler barfs, we'll remove that assumption
// and recompile again. See buildAndExec
func inferPackages(code string, pkgsToImport map[string]bool) {
	pkgs := pkgPat.FindAllString(code, -1)
	for _, pkg := range pkgs {
		pkg = pkg[:len(pkg)-1] // remove trailing '.'
		if importPkg, ok := builtinPkgs[pkg]; ok {
			pkgsToImport[importPkg] = true
		}
	}
}

func buildAndExec(topLevel string, nonTopLevel string, pkgsToImport map[string]bool) (out string, err string) {
	pkgsToImport["fmt"] = true // Explicitly imported in the template below in buildMain
	// If "fmt" is explicitly imported by the user, the compiler will flag a duplicate import error, and
	// repairImports takes care of the problem.
	src := buildMain(topLevel, nonTopLevel, pkgsToImport)
	out, err = run(src)
	if err != "" {
		if repairImports(err, pkgsToImport) {
			src = buildMain(topLevel, nonTopLevel, pkgsToImport)
			out, err = run(src)
		}
	}
	return out, err
}

// Look for compile errors of the form
//    "test.go:10: xxx redeclared as imported package name"
// and remove 'xxx' from pkgsToImport
// This is the most fragile part of this tool; it breaks if the compiler error message changes
func repairImports(err string, pkgsToImport map[string]bool) (dupsDetected bool) {
	dupsDetected = false
	var pkg string
	r := regexp.MustCompile(`(?m)(\w+) redeclared as imported package name|imported and not used: "(\w+)"`)
	for _, match := range r.FindAllStringSubmatch(err, -1) {
		// Either $1 or $2 will have name of pkg name that's been imported
		if match[1] != "" {
			pkg = match[1]
		} else if match[2] != "" {
			pkg = match[2]
		}
		if pkgsToImport[pkg] {
			// Was the duplicate import our mistake, due to an incorrect guess? If so ...
			delete(pkgsToImport, pkg)
			dupsDetected = true
		}
	}
	return dupsDetected
}

// save in a temp file, and "go run" it
func run(src string) (output string, err string) {
	tmpfile := save(src)
	cmd := exec.Command("go", "run", tmpfile)
	out, e := cmd.CombinedOutput()
	if e != nil {
		err = ""
		errPat := regexp.MustCompile(`^:(\d+)\[.*\]:(.*)$`)
		for _, e := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(e, "# command-line-arguments") {
				continue
			}
			err += errPat.ReplaceAllString(e, ":$1:$2\n")
		}
		return "", err
	} else {
		return string(out), ""
	}
	return "", ""
}

func save(src string) (tmpfile string) {
	tmpdir := os.Getenv("TMPDIR")
	if tmpdir == "" {
		tmpdir = os.Getenv("TEMPDIR")
	}
	if tmpdir == "" {
		tmpdir = os.TempDir()
	}
	tmpfile = path.Join(tmpdir, "gore_eval.go")
	fh, err := os.OpenFile(tmpfile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		panic("Unable to open file: '" + tmpfile + "': " + err.Error())
	}
	fh.WriteString(src)
	fh.Close()
	return tmpfile
}

func buildMain(topLevel string, nonTopLevel string, pkgsToImport map[string]bool) string {
	imports := ""
	for k, _ := range pkgsToImport {
		imports += `import "` + k + "\"\n"
	}
	template := `
package main
%s
%s
func main() {
%s
}

func __p(values ...interface{}){
	for _, v := range values {
             fmt.Printf("%%+v\n", v)
	}
}
func __t(values ...interface{}){
	for _, v := range values {
             fmt.Printf("%%T\n", v)
	}
}
`
	return fmt.Sprintf(template, imports, topLevel, nonTopLevel)
}

// Functions for converting the input string into a series of chunks.
//====================================================================

// Extract the next chunk from input. In case of an err, we attempt to
// package whatever's read so far into a chunk. After the final chunk is
// returned, the subsequent call to nextChunk returns an err=io.EOF indication
func nextChunk(scanner *Scanner) (chunk Chunk, err error) {
	// mark the current position. Used by mkChunk to extract a slice from
	// the input, starting from mark to the current read head
	mark := scanner.Mark()
	ch, err := scanner.ReadRune()

	if err != nil {
		return chunk, err
	}

	switch ch {
	case '/':
		// Is this the start of a single or multi-line comment?
		ch, err = scanner.ReadRune()
		if err != nil {
			return mkChunk(mark, scanner, KCOMMENT, 0, err)
		}
		switch ch {
		case '/':
			return readSingleLineComment(mark, scanner)
		case '*':
			return readMultilineComment(mark, scanner)
		default:
			return readText(mark, scanner)
		}
	case '"', '\'':
		return readString(mark, scanner, ch)
	case '`':
		return readMultilineString(mark, scanner)
	case '\n': // empty line
		return mkChunk(mark, scanner, KTEXT, 1, err)
	default:
		return readText(mark, scanner)
	}
	return
}

func readSingleLineComment(mark int, scanner *Scanner) (chunk Chunk, err error) {
	for {
		ch, err := scanner.ReadRune()
		if err != nil || ch == '\n' { // EOL or EOF or some other error, we'll package up what we have so far
			return mkChunk(mark, scanner, KCOMMENT, 1, err)
		}
	}
	return // dummy
}

func readMultilineComment(mark int, scanner *Scanner) (chunk Chunk, err error) {
	// "/*" has already been consumed. Read until EOF or until "*/", and count num of lines
	numLines := 0
	for {
		ch, err := scanner.ReadRune()
		if err != nil { // EOF or some other error, we'll package up what we have so far
			return mkChunk(mark, scanner, KCOMMENT, numLines, err)
		}
		switch ch {
		case '*':
			ch, err = scanner.ReadRune()
			if err != nil || ch == '/' {
				return mkChunk(mark, scanner, KCOMMENT, numLines, err)
			}
		case '\n':
			numLines++
		}
	}
	return
}

func readString(mark int, scanner *Scanner, endCh rune) (chunk Chunk, err error) {
	// Looking for endCh (single or double quote) while taking care of escapes
	for {
		ch, err := scanner.ReadRune()
		if err != nil { // EOL or EOF or some other error, we'll package up what we have so far
			return mkChunk(mark, scanner, KSTRING, 0, err)
		}
		if ch == endCh {
			return mkChunk(mark, scanner, KSTRING, 0, nil)
		} else if ch == '\\' {
			scanner.ReadRune() // read past next char
		} else if ch == '\n' {
			panic("Newline in string @ " + string(scanner.Pos()))
		}
	}
	return // dummy
}

func readMultilineString(mark int, scanner *Scanner) (chunk Chunk, err error) {
	numLines := 0
	for {
		ch, err := scanner.ReadRune()
		if err != nil { // EOF or some other error, we'll package up what we have so far
			return mkChunk(mark, scanner, KSTRING, 1, err)
		}
		switch ch {
		case '`':
			return mkChunk(mark, scanner, KSTRING, numLines, nil)
		case '\n':
			numLines++
		}
	}
	return // dummy
}

func readText(mark int, scanner *Scanner) (chunk Chunk, err error) {
	// read until EOL or EOF or string or possible beginning of comment
	for {
		ch, err := scanner.ReadRune()
		if err != nil { // EOF or some other error, we'll package up what we have so far
			return mkChunk(mark, scanner, KTEXT, 0, err)
		}
		switch ch {
		case '/':
			slashMark := scanner.Mark()
			ch, err = scanner.ReadRune()
			if ch == '*' || ch == '/' {
				// it is a comment.
				scanner.Reset(slashMark + 1) // The +1 is to unread the original slash as well
				return mkChunk(mark, scanner, KTEXT, 0, nil)
			}
		case '`', '"', '\'':
			scanner.UnreadRune() //  nextChunk will reprocess this character
			return mkChunk(mark, scanner, KTEXT, 0, nil)
		case '\n':
			return mkChunk(mark, scanner, KTEXT, 1, nil)
		}
	}
	return
}

func mkChunk(mark int, scanner *Scanner, kind int, numLines int, err error) (chunk Chunk, e error) {
	text := scanner.Slice(mark)
	if len(text) > 0 && err == io.EOF {
		err = nil // Delay the EOF until this chunk is processed;  Will get an EOF the next time
	}
	chunk = Chunk{text: text, kind: kind, numNL: numLines}
	//fmt.Printf("CHUNK : <<%+v>>\n", chunk)
	return chunk, err
}
