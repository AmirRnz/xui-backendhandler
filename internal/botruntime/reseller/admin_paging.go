package reseller

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/telebot.v3"
)

const adminReviewPageSize = 8
const maxAdminReviewPage = 100000

func parseAdminPage(raw string) (int, error) {
	page, err := strconv.Atoi(raw)
	if err != nil || page < 0 || page > maxAdminReviewPage {
		return 0, fmt.Errorf("invalid admin page")
	}
	return page, nil
}

func adminPageBounds(total, page int) (int, int, bool) {
	if total < 0 || page < 0 || page > maxAdminReviewPage || page > total/adminReviewPageSize {
		return 0, 0, false
	}
	start := page * adminReviewPageSize
	if start >= total {
		return 0, 0, false
	}
	end := start + adminReviewPageSize
	if end > total {
		end = total
	}
	return start, end, true
}

// actionPrefix is either "resellerpage" or "pendingpage|<kind>".
func adminPageNavigation(page, total int, actionPrefix string) []telebot.Btn {
	if total <= adminReviewPageSize {
		return nil
	}
	separator := "|"
	if strings.HasSuffix(actionPrefix, "|") {
		separator = ""
	}
	rows := make([]telebot.Btn, 0, 2)
	if page > 0 {
		rows = append(rows, btn("صفحه قبل", fmt.Sprintf("%s%s%d", actionPrefix, separator, page-1)))
	}
	if (page+1)*adminReviewPageSize < total {
		rows = append(rows, btn("صفحه بعد", fmt.Sprintf("%s%s%d", actionPrefix, separator, page+1)))
	}
	return rows
}
