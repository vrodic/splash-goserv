// Command tplcheck renders a template file with each JSON object line of
// stdin as its variables, printing {"out": ...} or {"error": ...} per line.
package main

import (
	"bufio"
	"fmt"
	"os"

	"splash/goserver/jinja"
	"splash/goserver/ojson"
)

func main() {
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	t, err := jinja.Parse(string(src))
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<20), 64<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for in.Scan() {
		v, err := ojson.Decode(in.Bytes())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		obj := v.(*ojson.Object)
		vars := map[string]jinja.Value{}
		for _, k := range obj.Keys() {
			vars[k] = jinja.FromJSON(obj.Value(k))
		}
		s, err := t.Render(vars)
		if err != nil {
			out.Write(ojson.Marshal(ojson.O("error", err.Error())))
		} else {
			out.Write(ojson.Marshal(ojson.O("out", s)))
		}
		out.WriteByte('\n')
	}
}
