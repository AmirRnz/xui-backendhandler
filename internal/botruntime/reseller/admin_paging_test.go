package reseller

import (
	"fmt"
	"testing"
)

func TestAdminReviewPagesExposeEveryPendingRecordWithoutGaps(t *testing.T) {
	for _, kind := range []string{"resellers", "payments", "topups"} {
		t.Run(kind, func(t *testing.T) {
			const total = adminReviewPageSize*3 + 3
			seen := make([]int, 0, total)
			for page := 0; ; page++ {
				start, end, ok := adminPageBounds(total, page)
				if !ok {
					break
				}
				if end-start > adminReviewPageSize {
					t.Fatalf("page %d has %d records", page, end-start)
				}
				for record := start; record < end; record++ {
					seen = append(seen, record)
				}
			}
			if len(seen) != total {
				t.Fatalf("reachable records = %d, want %d", len(seen), total)
			}
			for i, record := range seen {
				if record != i {
					t.Fatalf("position %d contains record %d; pages skipped or duplicated records", i, record)
				}
			}
		})
	}
}

func TestAdminReviewPageNavigationUsesStablePageIndices(t *testing.T) {
	const total = adminReviewPageSize*2 + 1
	for _, tc := range []struct {
		page int
		want []string
	}{
		{page: 0, want: []string{"resellerpage|1"}},
		{page: 1, want: []string{"resellerpage|0", "resellerpage|2"}},
		{page: 2, want: []string{"resellerpage|1"}},
	} {
		buttons := adminPageNavigation(tc.page, total, "resellerpage")
		got := make([]string, 0, len(buttons))
		for _, button := range buttons {
			got = append(got, button.Data)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("page %d navigation = %v, want %v", tc.page, got, tc.want)
		}
	}
	buttons := adminPageNavigation(1, total, "pendingpage|topup")
	if len(buttons) != 2 || buttons[0].Data != "pendingpage|topup|0" || buttons[1].Data != "pendingpage|topup|2" {
		t.Fatalf("top-up navigation = %#v", buttons)
	}
}

func TestAdminPageParsingAndBoundsRejectInvalidPage(t *testing.T) {
	for _, raw := range []string{"-1", "x", fmt.Sprint(maxAdminReviewPage + 1)} {
		if _, err := parseAdminPage(raw); err == nil {
			t.Errorf("parseAdminPage(%q) unexpectedly succeeded", raw)
		}
	}
	if _, _, ok := adminPageBounds(adminReviewPageSize, 1); ok {
		t.Fatal("page past the end unexpectedly exists")
	}
	if _, _, ok := adminPageBounds(adminReviewPageSize*2+1, 1); !ok {
		t.Fatal("middle page was rejected")
	}
}
