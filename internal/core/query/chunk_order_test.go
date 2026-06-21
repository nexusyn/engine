package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nexusyn/engine/internal/core/search"
)

func TestExtractSessionDate_StandardFormat(t *testing.T) {
	cases := map[string]time.Time{
		"[Session date: 2024/03/15]\nuser: hi": time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC),
		"[Session date: 2024-03-15]\nuser: hi": time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC),
		"[Date: 2025/12/01] notes":             time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
		"[Today: 2026-05-21] question":         time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		"[session date:  2023/01/05  ]":        time.Date(2023, 1, 5, 0, 0, 0, 0, time.UTC),
	}
	for content, expected := range cases {
		got := ExtractSessionDate(content)
		assert.Equal(t, expected, got, "content: %q", content)
	}
}

func TestExtractSessionDate_NoHeader_ReturnsZero(t *testing.T) {
	cases := []string{
		"user: hello there",
		"",
		"date is somewhere later in the text",
		"[Session: no date]",
		"[Session date: bad-format]",
	}
	for _, c := range cases {
		got := ExtractSessionDate(c)
		assert.True(t, got.IsZero(), "should be zero for %q, got %v", c, got)
	}
}

func TestExtractSessionDate_InvalidValues_ReturnsZero(t *testing.T) {
	cases := []string{
		"[Session date: 1800/13/45]", // year too old, month/day invalid
		"[Session date: 2024/13/01]", // month > 12
		"[Session date: 2024/00/15]", // month == 0
		"[Session date: 2024/05/32]", // day > 31
	}
	for _, c := range cases {
		got := ExtractSessionDate(c)
		assert.True(t, got.IsZero(), "should be zero for invalid date %q", c)
	}
}

func TestSortByDateDesc_OrdersMostRecentFirst(t *testing.T) {
	results := []search.Result{
		{ChunkID: 1, Content: "[Session date: 2024/01/15] old fact"},
		{ChunkID: 2, Content: "[Session date: 2024/06/20] mid fact"},
		{ChunkID: 3, Content: "[Session date: 2025/03/10] recent fact"},
	}
	SortByDateDesc(results)
	assert.Equal(t, int64(3), results[0].ChunkID, "most recent first")
	assert.Equal(t, int64(2), results[1].ChunkID)
	assert.Equal(t, int64(1), results[2].ChunkID, "oldest last")
}

func TestSortByDateDesc_NoDates_PreservesOrder(t *testing.T) {
	results := []search.Result{
		{ChunkID: 1, Content: "no header here"},
		{ChunkID: 2, Content: "also no date"},
		{ChunkID: 3, Content: "still no date"},
	}
	SortByDateDesc(results)
	assert.Equal(t, int64(1), results[0].ChunkID, "stable order preserved")
	assert.Equal(t, int64(2), results[1].ChunkID)
	assert.Equal(t, int64(3), results[2].ChunkID)
}

func TestSortByDateDesc_MixedDates_DatedFirst(t *testing.T) {
	results := []search.Result{
		{ChunkID: 1, Content: "[Session date: 2024/01/15] dated"},
		{ChunkID: 2, Content: "no date"},
		{ChunkID: 3, Content: "[Session date: 2025/03/10] more recent"},
		{ChunkID: 4, Content: "also no date"},
	}
	SortByDateDesc(results)
	// chunks com data primeiro (id=3, id=1), depois sem data (mantém ordem original 2, 4)
	assert.Equal(t, int64(3), results[0].ChunkID, "newest dated first")
	assert.Equal(t, int64(1), results[1].ChunkID, "older dated next")
	assert.Equal(t, int64(2), results[2].ChunkID, "first undated keeps order")
	assert.Equal(t, int64(4), results[3].ChunkID, "second undated keeps order")
}
