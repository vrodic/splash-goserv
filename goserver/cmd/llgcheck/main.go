// Command llgcheck loads llguidance and compiles a grammar, as a smoke test.
package main

import (
	"fmt"
	"os"

	"splash/goserver/llg"
	"splash/goserver/tokenizer"
)

func main() {
	if err := llg.Load(os.Args[1]); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("llguidance", llg.Version())
	data, _ := os.ReadFile(os.Args[2])
	tok, _ := tokenizer.Load(os.Args[2])
	t, err := llg.NewTokenizer(string(data), 248320, []uint32{248046, 248044}, tok.Encode)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	g := "%llguidance {}\nstart: \"yes\" | \"no\"\n"
	fmt.Println("validate:", t.Validate("lark", g) == "")
	m := t.NewMatcher("lark", g)
	mask := make([]uint32, m.MaskBytes()/4)
	m.ComputeMaskInto(mask)
	var allowed []string
	for i, w := range mask {
		for b := 0; b < 32; b++ {
			if w&(1<<b) != 0 {
				allowed = append(allowed, tok.Decode([]int{i*32 + b}, false))
			}
		}
	}
	fmt.Println("mask words", len(mask), "allowed", len(allowed), allowed[:min(10, len(allowed))])
	ids := tok.Encode("yes")
	fmt.Println("consume yes:", m.ConsumeTokens(toU32(ids)), "accepting:", m.IsAccepting())
	fmt.Println("bad:", t.Validate("lark", "start: ???"))
}

func toU32(a []int) []uint32 {
	out := make([]uint32, len(a))
	for i, v := range a {
		out[i] = uint32(v)
	}
	return out
}
