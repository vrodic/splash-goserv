// Command grammarcheck prints the constraint grammar of each JSON line of
// stdin, {"body": ..., "thinking": bool, "family": name}, as
// {"grammar": ...} or {"error": ...}.
package main

import (
	"bufio"
	"fmt"
	"os"

	"splash/goserver/llg"
	"splash/goserver/ojson"
	"splash/goserver/server"
	"splash/goserver/tokenizer"
)

func main() {
	if err := llg.Load(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	data, _ := os.ReadFile(os.Args[2])
	tok, _ := tokenizer.Load(os.Args[2])
	t, err := llg.NewTokenizer(string(data), 248320, []uint32{248046, 248044}, tok.Encode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	server.SetPatternValidator(func(g string) bool { return t.Validate("lark", g) == "" })
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
		o := v.(*ojson.Object)
		g, err := server.GrammarFor(o.Value("body").(*ojson.Object), o.Value("thinking") == true, o.Value("family").(string))
		if err != nil {
			out.Write(ojson.Marshal(ojson.O("error", err.Error())))
		} else {
			out.Write(ojson.Marshal(ojson.O("grammar", g)))
		}
		out.WriteByte('\n')
	}
}
