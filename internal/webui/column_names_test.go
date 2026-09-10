package webui

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A note under a table explains one of its columns, so it has to name a
// column the table has. The backups table's said "New" for every release
// after the column was renamed to "Stored", which sends a reader looking
// up and down a table for a heading that is not there.
func TestANoteUnderATableNamesAColumnTheTableHas(t *testing.T) {
	body, err := os.ReadFile("templates/partials.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)

	headings := map[string]bool{}
	for _, row := range regexp.MustCompile(`<thead>.*?</thead>`).FindAllString(page, -1) {
		for _, cell := range regexp.MustCompile(`<th[^>]*>([^<]*)</th>`).FindAllStringSubmatch(row, -1) {
			if name := strings.TrimSpace(cell[1]); name != "" && name != "&nbsp;" {
				headings[name] = true
			}
		}
	}
	if len(headings) == 0 {
		t.Fatal("no table headings were found, so this test proves nothing")
	}

	// A quoted word in a hint is this file's way of naming a column.
	quoted := regexp.MustCompile(`<p class="cpr-hint"[^>]*>[^<]*&#34;([A-Z][a-z]+)&#34;|"([A-Z][a-z]+)" is what`)
	found := false
	for _, match := range quoted.FindAllStringSubmatch(page, -1) {
		name := match[1]
		if name == "" {
			name = match[2]
		}
		found = true
		if !headings[name] {
			t.Errorf("a note names the column %q, and the table's headings are %v", name, keysOf(headings))
		}
	}
	if !found {
		t.Fatal("no note naming a column was found, so this test proves nothing")
	}
}

func keysOf(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	return names
}
