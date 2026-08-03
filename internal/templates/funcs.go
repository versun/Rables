package templates

import (
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"
)

// Flash carries the one-time notice/alert messages shown at the top of a
// page, mirroring the Rails flash. Template data passed to Render must expose
// it as a field or map key named Flash.
type Flash struct {
	Notice string
	Alert  string
}

// FuncMap returns the template functions available to all pages.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"formatTime":       FormatTime,
		"paginationWindow": PaginationWindow,
		"flashHTML":        FlashHTML,
	}
}

// FormatTime formats unix seconds (stored UTC) in the IANA time zone tzName
// (settings.time_zone semantics) using a Go reference-time layout. An unknown
// or empty zone falls back to UTC.
func FormatTime(unix int64, tzName, layout string) string {
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		loc = time.UTC
	}
	return time.Unix(unix, 0).In(loc).Format(layout)
}

// PageItem is one element of a pagination window: a page number, or a gap
// marker between distant ranges.
type PageItem struct {
	Page int  // 1-based page number; zero when Gap is true
	Gap  bool // renders an ellipsis instead of a page link
}

// will_paginate defaults, which the Rails app uses unmodified.
const (
	paginationInnerWindow = 4 // pages shown around the current page
	paginationOuterWindow = 1 // pages shown at the start and end
)

// PaginationWindow returns the visible page-number sequence for current
// (1-based) out of total pages, following will_paginate's window algorithm:
// the first/last outer-window pages are always shown, inner-window pages
// surround the current page, and collapsed ranges appear as gaps. current is
// clamped into [1, total]; total < 1 yields nil.
func PaginationWindow(current, total int) []PageItem {
	if total < 1 {
		return nil
	}
	if current < 1 {
		current = 1
	}
	if current > total {
		current = total
	}

	from := current - paginationInnerWindow
	to := current + paginationInnerWindow
	if to > total {
		from -= to - total
		to = total
	}
	if from < 1 {
		to += 1 - from
		from = 1
		if to > total {
			to = total
		}
	}

	items := make([]PageItem, 0, total)
	if paginationOuterWindow+3 < from { // gap between first pages and middle
		for p := 1; p <= paginationOuterWindow+1; p++ {
			items = append(items, PageItem{Page: p})
		}
		items = append(items, PageItem{Gap: true})
	} else {
		for p := 1; p < from; p++ {
			items = append(items, PageItem{Page: p})
		}
	}
	for p := from; p <= to; p++ {
		items = append(items, PageItem{Page: p})
	}
	if total-paginationOuterWindow-2 > to { // gap between middle and last pages
		items = append(items, PageItem{Gap: true})
		for p := total - paginationOuterWindow; p <= total; p++ {
			items = append(items, PageItem{Page: p})
		}
	} else {
		for p := to + 1; p <= total; p++ {
			items = append(items, PageItem{Page: p})
		}
	}
	return items
}

// FlashHTML renders the notice/alert flash messages, matching the Rails
// layout's flash markup (class hooks only; styling lives in CSS). Empty
// messages render nothing.
func FlashHTML(flash Flash) template.HTML {
	var b strings.Builder
	if flash.Notice != "" {
		fmt.Fprintf(&b, `<div class="flash flash-notice">%s</div>`, html.EscapeString(flash.Notice))
	}
	if flash.Alert != "" {
		fmt.Fprintf(&b, `<div class="flash flash-alert">%s</div>`, html.EscapeString(flash.Alert))
	}
	// Message text is escaped above; the wrapper markup is static.
	return template.HTML(b.String())
}
