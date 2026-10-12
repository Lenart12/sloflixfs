package fusefs

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

// name is the item's file name: the English one if available, which matches TMDB best.
func name(it sloflix.Item) string {
	if it.NameEn != "" {
		return clean(it.NameEn)
	}
	return clean(it.Name)
}

// clean removes characters unsafe in file names. Some upstream titles carry invisible zero-width characters.
func clean(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' {
			return '-'
		}
		if !unicode.IsGraphic(r) {
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}

// title is the Jellyfin folder name: "Name (Year)".
func title(it sloflix.Item) string {
	if it.Year > 0 {
		return fmt.Sprintf("%s (%d)", name(it), it.Year)
	}
	return name(it)
}

// uniqNames names each item, appending " [id]" to all but one of the items sharing a name. The plain name
// goes to the oldest playable one, so the path Jellyfin keeps watch history under stays when a duplicate
// is added or a dead title re-uploaded.
func uniqNames(items []sloflix.Item, name func(sloflix.Item) string, playable func(id int) bool) map[int]string {
	groups := map[string][]sloflix.Item{}
	for _, it := range items {
		groups[name(it)] = append(groups[name(it)], it)
	}
	out := map[int]string{}
	for n, g := range groups {
		slices.SortFunc(g, func(a, b sloflix.Item) int {
			if pa, pb := playable(a.ID), playable(b.ID); pa != pb {
				if pa {
					return -1
				}
				return 1
			}
			return a.ID - b.ID
		})
		for i, it := range g {
			out[it.ID] = n
			if i > 0 {
				out[it.ID] = fmt.Sprintf("%s [%d]", n, it.ID)
			}
		}
	}
	return out
}
