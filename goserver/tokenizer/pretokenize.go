package tokenizer

import (
	"unicode"
	"unicode/utf8"
)

// preTokenize splits text as the Split pre-tokenizer with expectedRegex and
// the Isolated behavior does: each match is a piece, and so is any text
// between matches. The alternatives are tried in order at each position,
// with the backtracking a Perl-style engine gives them.
func preTokenize(text string) []string {
	pieces := make([]string, 0, len(text)/4+1)
	gap := 0
	for i := 0; i < len(text); {
		n := matchAt(text, i)
		if n == 0 {
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
			continue
		}
		if gap < i {
			pieces = append(pieces, text[gap:i])
		}
		pieces = append(pieces, text[i:i+n])
		i += n
		gap = i
	}
	if gap < len(text) {
		pieces = append(pieces, text[gap:])
	}
	return pieces
}

func isLetter(r rune) bool { return unicode.IsLetter(r) }
func isNumber(r rune) bool { return unicode.IsNumber(r) }
func isMark(r rune) bool   { return unicode.IsMark(r) }
func isSpace(r rune) bool  { return unicode.IsSpace(r) }
func isLM(r rune) bool     { return isLetter(r) || isMark(r) }
func isNewline(r rune) bool {
	return r == '\r' || r == '\n'
}

func runeAt(s string, i int) (rune, int) {
	if i >= len(s) {
		return -1, 0
	}
	return utf8.DecodeRuneInString(s[i:])
}

// matchAt returns the length of the regex match starting at i, 0 for none.
func matchAt(s string, i int) int {
	if n := matchContraction(s, i); n > 0 {
		return n
	}
	if n := matchWord(s, i); n > 0 {
		return n
	}
	if r, size := runeAt(s, i); size > 0 && isNumber(r) {
		return size
	}
	if n := matchPunctuation(s, i); n > 0 {
		return n
	}
	if n := matchNewlines(s, i); n > 0 {
		return n
	}
	if n := matchTrailingSpace(s, i); n > 0 {
		return n
	}
	return spaceRun(s, i) - i
}

func lower(r rune) rune {
	if r == 'ſ' { // folds to s under case-insensitive matching
		return 's'
	}
	return unicode.ToLower(r)
}

// (?i:'s|'t|'re|'ve|'m|'ll|'d)
func matchContraction(s string, i int) int {
	if i >= len(s) || s[i] != '\'' {
		return 0
	}
	a, na := runeAt(s, i+1)
	if na == 0 {
		return 0
	}
	switch lower(a) {
	case 's', 't', 'm', 'd':
		return 1 + na
	}
	b, nb := runeAt(s, i+1+na)
	if nb == 0 {
		return 0
	}
	switch {
	case lower(a) == 'r' && lower(b) == 'e', lower(a) == 'v' && lower(b) == 'e', lower(a) == 'l' && lower(b) == 'l':
		return 1 + na + nb
	}
	return 0
}

// [^\r\n\p{L}\p{N}]?[\p{L}\p{M}]+
func matchWord(s string, i int) int {
	j := i
	if r, size := runeAt(s, j); size > 0 && !isNewline(r) && !isLetter(r) && !isNumber(r) {
		// The optional character; backtrack past it when no letter follows.
		if n := lmRun(s, j+size); n > j+size {
			return n - i
		}
	}
	if n := lmRun(s, j); n > j {
		return n - i
	}
	return 0
}

func lmRun(s string, j int) int {
	for {
		r, size := runeAt(s, j)
		if size == 0 || !isLM(r) {
			return j
		}
		j += size
	}
}

// ?[^\s\p{L}\p{M}\p{N}]+[\r\n]*
func matchPunctuation(s string, i int) int {
	try := func(j int) int {
		k := j
		for {
			r, size := runeAt(s, k)
			if size == 0 || isSpace(r) || isLM(r) || isNumber(r) {
				break
			}
			k += size
		}
		if k == j {
			return 0
		}
		for k < len(s) && (s[k] == '\r' || s[k] == '\n') {
			k++
		}
		return k
	}
	if i < len(s) && s[i] == ' ' {
		if k := try(i + 1); k > 0 {
			return k - i
		}
	}
	if k := try(i); k > 0 {
		return k - i
	}
	return 0
}

func spaceRun(s string, i int) int {
	j := i
	for {
		r, size := runeAt(s, j)
		if size == 0 || !isSpace(r) {
			return j
		}
		j += size
	}
}

// \s*[\r\n]+ : the whitespace run up to and including its last line break.
func matchNewlines(s string, i int) int {
	end := spaceRun(s, i)
	last := -1
	for j := i; j < end; j++ {
		if s[j] == '\r' || s[j] == '\n' {
			last = j
		}
	}
	if last < 0 {
		return 0
	}
	return last + 1 - i
}

// \s+(?!\S): the whole run at the end of the text, else the run but its
// last character.
func matchTrailingSpace(s string, i int) int {
	end := spaceRun(s, i)
	if end == i {
		return 0
	}
	if end == len(s) {
		return end - i
	}
	// Drop the run's last character, which the next non-space follows.
	_, size := utf8.DecodeLastRuneInString(s[i:end])
	if end-size > i {
		return end - size - i
	}
	return 0
}
