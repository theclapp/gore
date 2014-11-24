package main

import (
	"bufio"
	"fmt"
	"github.com/theclapp/gore/eval"
	"io"
	"os"
)

func main() {
	var src string
	if len(os.Args) > 1 {
		src = os.Args[1]
	} else {
		fmt.Println("Enter one or more lines and hit ctrl-D")
		src = readStdin()
	}

	out, err := eval.Eval(src)
	if err == "" {
		fmt.Fprint(os.Stdout, out)
	} else {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}
}

func readStdin() (buf string) {
	r := bufio.NewReader(os.Stdin)
	for {
		if line, err := r.ReadString('\n'); err != nil {
			if err == io.EOF {
				buf += line
			}
			break
		} else {
			buf += line
		}
	}
	return buf
}
