// Command tokcheck encodes each JSON string line of stdin and prints its ids.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"splash/goserver/tokenizer"
)

func main() {
	t, err := tokenizer.Load(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<20), 64<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for in.Scan() {
		var s string
		if err := json.Unmarshal(in.Bytes(), &s); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ids := t.Encode(s)
		b, _ := json.Marshal(map[string]any{"ids": ids, "text": t.Decode(ids, true)})
		out.Write(b)
		out.WriteByte('\n')
	}
}
