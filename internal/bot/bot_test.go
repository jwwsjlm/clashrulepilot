package bot

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"clashrulepilot/internal/lookup"
	"clashrulepilot/internal/rules"
	"github.com/go-telegram/bot/models"
)

func TestMainMenuUsesInlineKeyboard(t *testing.T) {
	menu := mainMenu()
	rows := menu.InlineKeyboard
	if len(rows) != 2 || len(rows[0]) != 2 || len(rows[1]) != 2 {
		t.Fatalf("expected a 2x2 menu grid, got %#v", rows)
	}
	count := 0
	want := map[string]bool{
		"menu:query": false, "menu:status": false, "menu:repo": false, "menu:help": false,
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
	if count != 4 {
		t.Fatalf("expected 4 menu buttons, got %d", count)
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
	if strings.Contains(string(data), "menu:remove") {
		t.Fatalf("delete must not be present in the main menu: %s", data)
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

func TestHelpIncludesPersonalRuleTutorial(t *testing.T) {
	b := &Bot{}
	text := b.helpText()
	for _, want := range []string{
		"📘 个人规则接入 OpenClash",
		"服务 → OpenClash → 运行状态 → 顶部「覆写模块」按钮",
		"Subscribe",
		"config=all",
		"点击模块刷新后，重启 OpenClash",
		"+rules",
		"系统 → 软件包",
		"服务 → OpenClash → 插件设置 → 调试日志 → 生成",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("help text missing tutorial item %q: %s", want, text)
		}
	}
	if len([]rune(text)) >= 4096 {
		t.Fatalf("help text must fit Telegram message limit, got %d runes", len([]rune(text)))
	}
}

func TestCancelMenuIsSingleStepToMainMenu(t *testing.T) {
	menu := cancelMenu()
	if len(menu.InlineKeyboard) != 1 || len(menu.InlineKeyboard[0]) != 1 {
		t.Fatalf("cancel menu should contain one unambiguous action: %#v", menu)
	}
	button := menu.InlineKeyboard[0][0]
	if button.CallbackData != "nav:cancel" || button.Text != "✖️ 取消并返回主菜单" {
		t.Fatalf("unexpected cancel button: %#v", button)
	}
	if strings.Contains(button.Text, "已取消") {
		t.Fatal("cancel button must not depend on an intermediate cancellation message")
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
	labels := make([]string, 0)
	for _, row := range menu.InlineKeyboard {
		for _, item := range row {
			labels = append(labels, item.Text)
		}
	}
	for _, want := range []string{"🎯 仅当前域名", "🌿 当前域名及下级 · ⭐ 推荐", "🌐 整个主域名", "🧰 高级匹配", "✖️ 取消并返回主菜单"} {
		found := false
		for _, label := range labels {
			if label == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing normalized button label %q: %#v", want, labels)
		}
	}
	for _, want := range []string{"🧭 请选择规则覆盖范围", "🔍 输入域名：", "🛡️ 安全主域名：", "📋 规则：", "✅ 优点：", "⚠️ 注意：", "⭐ 推荐"} {
		if !strings.Contains(text, want) {
			t.Fatalf("match menu is missing standardized label %q: %s", want, text)
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

func TestRemovalConfirmationListsEveryAffectedRule(t *testing.T) {
	store := rules.Store{Rules: []rules.Rule{
		{Domain: "example.com", Match: rules.Exact, Action: rules.Direct},
		{Domain: "example.com", Match: rules.Suffix, Action: rules.Proxy},
		{Domain: "other.example", Match: rules.Exact, Action: rules.Direct},
	}}
	matched := rulesForDomain(store, "example.com")
	if len(matched) != 2 {
		t.Fatalf("expected 2 affected rules, got %d", len(matched))
	}
	text := removalConfirmationText("example.com", matched)
	for _, want := range []string{"删除规则确认（第 2 步）", "DOMAIN,example.com", "DOMAIN-SUFFIX,example.com", "不可在 Bot 中直接撤销"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in confirmation: %s", want, text)
		}
	}
}

func TestSameRuleSetDetectsConfirmationRace(t *testing.T) {
	original := []rules.Rule{{Domain: "example.com", Match: rules.Exact, Action: rules.Direct}}
	if !sameRuleSet(original, append([]rules.Rule(nil), original...)) {
		t.Fatal("identical snapshots should match")
	}
	changed := []rules.Rule{{Domain: "example.com", Match: rules.Exact, Action: rules.Proxy}}
	if sameRuleSet(original, changed) {
		t.Fatal("group change between preview and confirmation must be detected")
	}
}

func TestDNSReportUsesCompactLineSpacing(t *testing.T) {
	text := formatDNSReport(lookup.Report{
		LocalA:   []string{"198.18.0.1"},
		FakeIP:   []string{"198.18.0.1"},
		Domestic: lookup.DNSGroupResult{Group: "domestic", Provider: "dns.alidns.com", A: []string{"1.1.1.1"}},
		Foreign:  lookup.DNSGroupResult{Group: "foreign", Provider: "cloudflare-dns.com", A: []string{"8.8.8.8"}},
		A:        []string{"1.1.1.1", "8.8.8.8"},
	})
	if strings.Contains(text, "\n\n") {
		t.Fatalf("DNS summary contains extra blank lines: %q", text)
	}
}

func TestWhoisEntitiesLinkQueryAndRegistrableDomains(t *testing.T) {
	const (
		queried     = "rdap.publicinterestregistry.org"
		registrable = "publicinterestregistry.org"
	)
	text := "🔍 查询域名：" + queried + "\n可注册域名：" + registrable
	entities := whoisEntities(text, queried, registrable)
	if len(entities) != 2 {
		t.Fatalf("expected two WHOIS links, got %#v", entities)
	}
	wantURLs := []string{
		"https://who.is/whois/rdap.publicinterestregistry.org",
		"https://who.is/whois/publicinterestregistry.org",
	}
	for index, entity := range entities {
		if entity.Type != models.MessageEntityTypeTextLink {
			t.Fatalf("entity %d is not a text link: %#v", index, entity)
		}
		if entity.URL != wantURLs[index] {
			t.Fatalf("entity %d URL=%q want=%q", index, entity.URL, wantURLs[index])
		}
	}
	if entities[0].Offset != telegramTextLength("🔍 查询域名：") || entities[0].Length != telegramTextLength(queried) {
		t.Fatalf("unexpected queried-domain entity offsets: %#v", entities[0])
	}
	rootPrefix := "🔍 查询域名：" + queried + "\n可注册域名："
	if entities[1].Offset != telegramTextLength(rootPrefix) || entities[1].Length != telegramTextLength(registrable) {
		t.Fatalf("unexpected registrable-domain entity offsets: %#v", entities[1])
	}
}

func TestWhoisEntitiesSkipUnknownRegistrableDomain(t *testing.T) {
	text := "🔍 查询域名：example.com\n可注册域名：未识别"
	entities := whoisEntities(text, "example.com", "未识别")
	if len(entities) != 1 {
		t.Fatalf("unknown registrable domain must not be linked: %#v", entities)
	}
}

func TestDNSDetailsHaveExplicitSectionBreaks(t *testing.T) {
	var out strings.Builder
	shown := 0
	appendDNSGroupDetails(&out, lookup.DNSGroupResult{
		Provider: "dns.alidns.com",
		A:        []string{"108.157.254.58"},
	}, map[string]lookup.GeoIPInfo{
		"108.157.254.58": {IP: "108.157.254.58", CountryCode: "SG", Country: "Singapore"},
	}, &shown)
	text := out.String()
	for _, want := range []string{"状态：主端点\n\n解析记录：\n1. A", "IP：108.157.254.58", "🇸🇬 Singapore（新加坡）"} {
		if !strings.Contains(text, want) {
			t.Fatalf("DNS details missing explicit break %q: %q", want, text)
		}
	}
}

func TestGeoSuggestionColorsOnlyRoutingKeywords(t *testing.T) {
	proxyText, proxyKind := geoSuggestion(lookup.Report{ChinaChecked: 1, GeoIPs: []lookup.GeoIPInfo{{IP: "8.8.8.8", CountryCode: "US"}}})
	if proxyKind != "proxy" || !strings.Contains(proxyText, "🔴 代理") || strings.Contains(proxyText, "🟢") {
		t.Fatalf("unexpected proxy suggestion: %q kind=%q", proxyText, proxyKind)
	}
	directText, directKind := geoSuggestion(lookup.Report{ChinaChecked: 1, China: true, GeoIPs: []lookup.GeoIPInfo{{IP: "1.2.3.4", CountryCode: "CN", China: true}}})
	if directKind != "direct" || !strings.Contains(directText, "🟢 直连") {
		t.Fatalf("unexpected direct suggestion: %q kind=%q", directText, directKind)
	}
}

func TestCountryFlag(t *testing.T) {
	cases := map[string]string{
		"US":   "🇺🇸",
		"cn":   "🇨🇳",
		" JP ": "🇯🇵",
		"":     "🌐",
		"USA":  "🌐",
		"1A":   "🌐",
	}
	for input, want := range cases {
		if got := countryFlag(input); got != want {
			t.Fatalf("countryFlag(%q)=%q want %q", input, got, want)
		}
	}
}

func TestGeoPlaceIncludesCountryFlag(t *testing.T) {
	got := geoPlace(lookup.GeoIPInfo{
		Country: "United States", CountryCode: "US", Region: "California", City: "San Francisco", ISP: "Cloudflare, Inc.", ASN: "13335",
	})
	for _, want := range []string{"🇺🇸 United States", "California", "San Francisco", "ISP Cloudflare, Inc.", "AS13335"} {
		if !strings.Contains(got, want) {
			t.Fatalf("geoPlace missing %q: %s", want, got)
		}
	}
}

func TestCountryLabelIncludesChineseNameAndMaintainedFlag(t *testing.T) {
	got := countryLabel("US", "United States")
	for _, want := range []string{"🇺🇸", "United States", "美国"} {
		if !strings.Contains(got, want) {
			t.Fatalf("countryLabel missing %q: %s", want, got)
		}
	}
}

func TestSmartSuggestionDoesNotRepeatExistingProxyRule(t *testing.T) {
	decision := smartSuggestion([]rules.Rule{{Domain: "example.com", Match: rules.Suffix, Action: rules.Proxy}}, lookup.Report{
		ChinaChecked: 1,
		GeoIPs:       []lookup.GeoIPInfo{{IP: "8.8.8.8", CountryCode: "US"}},
	})
	if decision.ButtonAction != rules.Direct {
		t.Fatalf("expected optional switch to direct, got %+v", decision)
	}
	if !strings.Contains(decision.Text, "当前个人规则已经是🔴 代理") || strings.Contains(decision.ButtonText, "添加代理") {
		t.Fatalf("suggestion repeated the existing proxy action: %+v", decision)
	}
	if decision.ButtonText != "🟢 切换为直连（不建议）" {
		t.Fatalf("unexpected switch label: %q", decision.ButtonText)
	}
}

func TestProxySuggestionButtonIsMarkedRecommended(t *testing.T) {
	decision := smartSuggestion(nil, lookup.Report{
		ChinaChecked: 1,
		GeoIPs:       []lookup.GeoIPInfo{{IP: "8.8.8.8", CountryCode: "US"}},
	})
	if decision.ButtonAction != rules.Proxy || decision.ButtonText != "🔴 建议添加代理 · ⭐ 推荐" {
		t.Fatalf("proxy recommendation is not clearly marked: %+v", decision)
	}
}

func TestQueryUsesActivePromptMessageAsEditTarget(t *testing.T) {
	p := &pending{Mode: "query", ActiveMessageID: 1234}
	target := queryMessageTarget(p, nil)
	if target == nil || target.ID != 1234 {
		t.Fatalf("query should edit the active prompt message: %#v", target)
	}
	existing := &models.Message{ID: 5678}
	if got := queryMessageTarget(p, existing); got != existing {
		t.Fatalf("explicit callback message target must be preserved: %#v", got)
	}
}

func TestSmartSuggestionKeepsMatchingPersonalRuleWithoutButton(t *testing.T) {
	decision := smartSuggestion([]rules.Rule{{Domain: "example.com", Match: rules.Exact, Action: rules.Direct}}, lookup.Report{
		ChinaChecked: 1,
		China:        true,
		GeoIPs:       []lookup.GeoIPInfo{{IP: "1.2.3.4", CountryCode: "CN", China: true}},
	})
	if decision.ButtonAction != "" || !strings.Contains(decision.Text, "无需重复添加") {
		t.Fatalf("matching personal rule should be kept without duplicate action: %+v", decision)
	}
}

func TestPageStackKeepsMostRecentFivePages(t *testing.T) {
	b := &Bot{sessions: map[int64]*pending{1: {Mode: "view"}}}
	for i := 0; i < 8; i++ {
		b.recordPage(1, i+1, string(rune('A'+i)), homeEditMenu(), nil, true)
	}
	p := b.sessions[1]
	if len(p.PageStack) != 5 {
		t.Fatalf("expected five page snapshots, got %d", len(p.PageStack))
	}
	if p.PageStack[0].Text != "C" || p.PageStack[4].Text != "G" {
		t.Fatalf("page stack did not retain the most recent pages: %+v", p.PageStack)
	}
}

func TestCallbackDedupeIsConcurrentSafe(t *testing.T) {
	b := &Bot{seenCallbacks: map[string]time.Time{}}
	var wg sync.WaitGroup
	var accepted int
	var mu sync.Mutex
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !b.callbackAlreadyHandled("same-callback") {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("callback should be accepted once, got %d", accepted)
	}
}

func TestSessionOwnerIsolation(t *testing.T) {
	b := &Bot{sessions: map[int64]*pending{}}
	if !b.bindSessionOwner(100, 1) {
		t.Fatal("first owner should bind")
	}
	if b.bindSessionOwner(100, 2) {
		t.Fatal("another user must not take over the same session")
	}
	if !b.bindSessionOwner(200, 2) {
		t.Fatal("different chat should remain isolated")
	}
}
