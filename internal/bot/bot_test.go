package bot

import (
	"encoding/json"
	"strings"
	"testing"

	"clashrulepilot/internal/rules"
)

func TestMainMenuUsesInlineKeyboard(t *testing.T) {
	menu := mainMenu()
	rows := menu.InlineKeyboard
	if len(rows) != 4 {
		t.Fatalf("expected 4 menu rows, got %d", len(rows))
	}
	count := 0
	want := map[string]bool{
		"menu:query": false, "menu:add:direct": false, "menu:add:proxy": false, "menu:remove": false,
		"menu:list": false, "menu:status": false, "menu:repo": false, "menu:help": false,
	}
	for _, row := range rows {
		count += len(row)
		for _, item := range row {
			if item.Text == "" || item.CallbackData == "" {
				t.Fatalf("button is incomplete: %+v", item)
			}
			if _, ok := want[item.CallbackData]; !ok {
				t.Fatalf("unexpected callback data %q", item.CallbackData)
			}
			want[item.CallbackData] = true
		}
	}
	if count != 8 {
		t.Fatalf("expected 8 menu buttons, got %d", count)
	}
	for callback, found := range want {
		if !found {
			t.Fatalf("missing callback %q", callback)
		}
	}
	data, err := json.Marshal(menu)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" {
		t.Fatal("menu JSON is empty")
	}
	if !strings.Contains(string(data), `"callback_data":"menu:query"`) {
		t.Fatalf("menu JSON is missing Telegram callback_data: %s", data)
	}
}

func TestBusyGuardRejectsRepeatedAction(t *testing.T) {
	b := &Bot{sessions: map[int64]*pending{7: {Mode: "add"}}}
	if !b.markBusy(7) {
		t.Fatal("first action should acquire busy guard")
	}
	if b.markBusy(7) {
		t.Fatal("repeated action must not acquire busy guard")
	}
	b.setBusy(7, false)
	if !b.markBusy(7) {
		t.Fatal("guard should be reusable after conflict confirmation")
	}
}

func TestHomeMenuUsesInlineKeyboard(t *testing.T) {
	menu := homeMenu()
	rows := menu.InlineKeyboard
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].CallbackData != "nav:home" {
		t.Fatalf("unexpected home menu: %#v", menu)
	}
}

func TestSmartMatchMenuForSubdomain(t *testing.T) {
	p := &pending{OriginalDomain: "cdn.legendsen.se", Domain: "cdn.legendsen.se", RootDomain: "legendsen.se"}
	text, menu := matchMenuContent(p)
	for _, want := range []string{"DOMAIN,cdn.legendsen.se", "DOMAIN-SUFFIX,cdn.legendsen.se", "DOMAIN-SUFFIX,legendsen.se"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing explanation %q in %s", want, text)
		}
	}
	wantCallbacks := map[string]bool{"match:exact": false, "match:suffix": false, "match:root": false, "match:advanced": false, "nav:cancel": false}
	for _, row := range menu.InlineKeyboard {
		for _, item := range row {
			if _, ok := wantCallbacks[item.CallbackData]; ok {
				wantCallbacks[item.CallbackData] = true
			}
		}
	}
	for callback, found := range wantCallbacks {
		if !found {
			t.Fatalf("missing callback %s", callback)
		}
	}
}

func TestSmartMatchMenuForRootHasNoDuplicateRootButton(t *testing.T) {
	p := &pending{OriginalDomain: "legendsen.se", Domain: "legendsen.se", RootDomain: "legendsen.se"}
	_, menu := matchMenuContent(p)
	rootCount := 0
	for _, row := range menu.InlineKeyboard {
		for _, item := range row {
			if item.CallbackData == "match:root" {
				rootCount++
			}
		}
	}
	if rootCount != 0 {
		t.Fatalf("root input must not include duplicate match:root button")
	}
}

func TestSmartMatchMenuHidesUnsafeSuffixForPublicSuffix(t *testing.T) {
	p := &pending{OriginalDomain: "co.uk", Domain: "co.uk"}
	text, menu := matchMenuContent(p)
	if !strings.Contains(text, "已隐藏后缀范围") {
		t.Fatalf("missing public suffix warning: %s", text)
	}
	for _, row := range menu.InlineKeyboard {
		for _, item := range row {
			if item.CallbackData == "match:suffix" || item.CallbackData == "match:root" {
				t.Fatalf("unsafe suffix callback was displayed: %s", item.CallbackData)
			}
		}
	}
}

func TestAdvancedSuggestions(t *testing.T) {
	if got := advancedSuggestion(rules.Keyword, "gw2.oops.asia", "oops.asia"); got != "gw2" {
		t.Fatalf("keyword suggestion=%q", got)
	}
	if got := advancedSuggestion(rules.Wildcard, "gw2.oops.asia", "oops.asia"); got != "*.oops.asia" {
		t.Fatalf("wildcard suggestion=%q", got)
	}
	if got := advancedSuggestion(rules.Regex, "gw2.oops.asia", "oops.asia"); got != `(^|\.)oops\.asia$` {
		t.Fatalf("regex suggestion=%q", got)
	}
}
