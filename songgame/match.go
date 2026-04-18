package main

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	parenRE = regexp.MustCompile(`\s*[\(\[][^)\]]*[\)\]]`)
	featRE  = regexp.MustCompile(`(?i)\s+(feat\.?|ft\.?|featuring|with)\s+.*$`)
	dashRE  = regexp.MustCompile(`\s+-\s+.*$`) // e.g. "Song - Remastered 2011"
)

// normalize strips parenthetical/bracketed content, trailing "- remaster" annotations,
// "feat." credits, accents, punctuation, and leading articles; lowercases & collapses whitespace.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = parenRE.ReplaceAllString(s, "")
	s = dashRE.ReplaceAllString(s, "")
	s = featRE.ReplaceAllString(s, "")

	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		}
	}
	s = b.String()
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimPrefix(s, "the ")
	s = strings.TrimPrefix(s, "a ")
	s = strings.TrimPrefix(s, "an ")
	return s
}

func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// fuzzyMatch returns true if guess is close enough to actual (after normalization).
// Allows exact match, substring containment either way, or small edit distance relative to length.
func fuzzyMatch(guess, actual string) bool {
	g := normalize(guess)
	a := normalize(actual)
	if g == "" || a == "" {
		return false
	}
	if g == a {
		return true
	}
	// containment only counts if the shorter side is reasonably long, to avoid "a" matching everything
	shortest := len(g)
	if len(a) < shortest {
		shortest = len(a)
	}
	if shortest >= 4 && (strings.Contains(g, a) || strings.Contains(a, g)) {
		return true
	}
	// edit distance tolerance scales with length
	tol := 1
	if shortest >= 8 {
		tol = 2
	}
	if shortest >= 14 {
		tol = 3
	}
	return levenshtein(g, a) <= tol
}

// matchAnyArtist returns true if guess matches any single artist of a multi-artist track.
func matchAnyArtist(guess string, artists []string) bool {
	for _, a := range artists {
		if fuzzyMatch(guess, a) {
			return true
		}
	}
	// Also try matching the joined form, e.g. "simon and garfunkel"
	joined := strings.Join(artists, " ")
	return fuzzyMatch(guess, joined)
}
