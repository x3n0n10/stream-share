package server

import (
	"encoding/json"
	"testing"

	"github.com/lucasduport/stream-share/pkg/filter"
)

func TestCatalogStoreOfWrapsRawSlices(t *testing.T) {
	n := 0
	s := catalogStoreOf([]interface{}{map[string]interface{}{"stream_id": "1"}, "skipped"})
	if err := s.each(func(m map[string]interface{}) bool { n++; return true }); err != nil || n != 1 {
		t.Fatalf("slice: n=%d err=%v", n, err)
	}
	n = 0
	if err := catalogStoreOf("x").each(func(m map[string]interface{}) bool { n++; return true }); err != nil || n != 0 {
		t.Fatalf("string: n=%d err=%v", n, err)
	}
	if catalogStoreOf(s) != s {
		t.Fatal("a store must be returned as is")
	}
}

func TestKeepCatalogEntryMatchesFilterEntries(t *testing.T) {
	c := &Config{}
	c.catalogFilter, _ = filter.NewSet(filter.Config{All: filter.Patterns{Group: `^A\|`, Name: `^A\|`}, ExcludeAdult: true})
	names := map[string]string{"1": "A| News", "2": "B| Sport"}
	items := []interface{}{
		map[string]interface{}{"name": "A| One", "category_id": "1", "is_adult": 0},
		map[string]interface{}{"name": "A| Two", "category_id": "2", "is_adult": 0},
		map[string]interface{}{"name": "B| Three", "category_id": "1", "is_adult": 0},
		map[string]interface{}{"name": "A| Four", "category_id": "1", "is_adult": 1},
	}
	want := c.filterEntries(filter.Live, items, names)
	var got []interface{}
	for _, it := range items {
		if c.keepCatalogEntry(filter.Live, it.(map[string]interface{}), names) {
			got = append(got, it)
		}
	}
	if len(want) != 1 || len(got) != 1 || got[0].(map[string]interface{})["name"] != "A| One" {
		t.Fatalf("filterEntries=%v keepCatalogEntry=%v", want, got)
	}
}

func TestInCategoryMatchesNumericIDs(t *testing.T) {
	if !inCategory(map[string]interface{}{"category_id": json.Number("42")}, "42") {
		t.Fatal("numeric category_id not matched")
	}
	m := map[string]interface{}{"category_id": "1", "category_ids": []interface{}{json.Number("7")}}
	if !inCategory(m, "7") || inCategory(m, "8") {
		t.Fatal("category_ids matching wrong")
	}
}

func TestChannelHarvestCollectsNamesAndEPG(t *testing.T) {
	c := &Config{}
	h := newChannelHarvest()
	h.add(map[string]interface{}{"stream_id": "9001", "name": " Alpha ", "epg_channel_id": "a.epg"})
	h.add(map[string]interface{}{"stream_id": json.Number("9002"), "name": "Beta"})
	if n := c.applyChannelHarvest(h); n != 2 {
		t.Fatalf("indexed %d, want 2", n)
	}
	for id, want := range map[string]string{"9001": "Alpha", "9002": "Beta"} {
		if got, ok := lookupAPIChannelName(normalizeStreamID(id)); !ok || got != want {
			t.Fatalf("%s: got %q %v, want %q", id, got, ok, want)
		}
	}
	if h.epgIDs["9001"] != "a.epg" {
		t.Fatalf("epg ids = %v", h.epgIDs)
	}
}
