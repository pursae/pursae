package store

import (
	"strconv"
	"strings"
	"testing"
)

func TestFixsecHTTPNormalizeSearch(t *testing.T) {
	s, ex := normalizeSearch("apple -pear apple banana -x")
	if s != "apple banana" || len(ex) != 2 || ex[0] != "pear" || ex[1] != "x" {
		t.Errorf("got %q %v", s, ex)
	}
	if s, _ := normalizeSearch(strings.Repeat("a", 1000)); len([]rune(s)) != searchMaxRunes {
		t.Errorf("length not capped: %d", len(s))
	}
	var words []string
	for i := range 100 {
		words = append(words, "w"+strconv.Itoa(i))
	}
	if s, _ := normalizeSearch(strings.Join(words, " ")); len(strings.Fields(s)) != searchMaxTerms {
		t.Errorf("terms not capped: %d", len(strings.Fields(s)))
	}
}
