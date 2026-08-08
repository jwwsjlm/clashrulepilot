package bot

import (
	"encoding/json"
	"strings"
	"testing"
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
	b := &Bot{sessions: map[int64]*pending{7: {Mode: "add"}}, removed: map[int64]bool{}}
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
