package bot

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"clashrulepilot/internal/app"
	"clashrulepilot/internal/config"
	"clashrulepilot/internal/domain"
	"clashrulepilot/internal/lookup"
	"clashrulepilot/internal/ruleindex"
	"clashrulepilot/internal/rules"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Bot struct {
	service  *app.Service
	cfg      config.Config
	api      *tgbot.Bot
	sessions map[int64]*pending
	mu       sync.Mutex
}

type pending struct {
	Mode            string
	OriginalDomain  string
	Domain          string
	RootDomain      string
	Action          rules.Action
	Match           rules.Match
	AdvancedMatch   rules.Match
	AdvancedSuggest string
	AwaitingPattern bool
	Busy            bool
	Candidates      []string
}

type button struct {
	Text string
	Data string
}

func New(service *app.Service, cfg config.Config) (*Bot, error) {
	b := &Bot{
		service:  service,
		cfg:      cfg,
		sessions: make(map[int64]*pending),
	}
	api, err := tgbot.New(cfg.TelegramToken,
		tgbot.WithDefaultHandler(b.handle),
		tgbot.WithErrorsHandler(func(err error) { log.Printf("telegram library: %v", err) }),
		tgbot.WithCheckInitTimeout(10*time.Second),
		tgbot.WithAllowedUpdates(tgbot.AllowedUpdates{"message", "callback_query"}),
		tgbot.WithNotAsyncHandlers(),
	)
	if err != nil {
		return nil, err
	}
	b.api = api
	return b, nil
}

func (b *Bot) Run(ctx context.Context) {
	me, err := b.api.GetMe(ctx)
	if err != nil {
		log.Printf("telegram startup check failed: %v", err)
		return
	}
	log.Printf("telegram connected bot=@%s id=%d allowlist=%v", me.Username, me.ID, b.cfg.Allowlist)
	b.api.Start(ctx)
}

func (b *Bot) allowed(id int64, private bool) bool { return private && b.cfg.Allowlist[id] }

func (b *Bot) handle(ctx context.Context, _ *tgbot.Bot, item *models.Update) {
	if item == nil {
		return
	}
	if item.CallbackQuery != nil {
		log.Printf("telegram callback received from=%d data=%q", item.CallbackQuery.From.ID, item.CallbackQuery.Data)
		b.handleCallback(ctx, item.CallbackQuery)
		return
	}
	if item.Message == nil || item.Message.From == nil {
		log.Printf("telegram update ignored: message or sender missing")
		return
	}
	if !b.allowed(item.Message.From.ID, item.Message.Chat.Type == models.ChatTypePrivate) {
		log.Printf("telegram message rejected from=%d chat_type=%s", item.Message.From.ID, item.Message.Chat.Type)
		return
	}
	text := strings.TrimSpace(item.Message.Text)
	log.Printf("telegram message accepted update_id=%d from=%d chat=%d text=%q", item.ID, item.Message.From.ID, item.Message.Chat.ID, text)
	if b.handlePendingText(ctx, item.Message.Chat.ID, text) {
		return
	}
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return
	}
	switch strings.ToLower(parts[0]) {
	case "/start", "/help":
		b.send(ctx, item.Message.Chat.ID, b.welcomeText(), mainMenu())
	case "/add":
		b.addStart(ctx, item.Message.Chat.ID, parts)
	case "/query":
		if len(parts) < 2 {
			b.startQueryMode(ctx, item.Message.Chat.ID)
		} else if candidates, err := domain.Extract(strings.Join(parts[1:], " ")); err != nil {
			b.send(ctx, item.Message.Chat.ID, "域名无效："+err.Error(), homeMenu())
		} else if len(candidates) == 1 {
			b.query(ctx, item.Message.Chat.ID, candidates[0])
		} else {
			b.mu.Lock()
			b.sessions[item.Message.Chat.ID] = &pending{Mode: "query", Candidates: candidates}
			b.mu.Unlock()
			rows := make([][]button, 0, len(candidates)+1)
			for n, candidate := range candidates {
				if n >= 8 {
					break
				}
				rows = append(rows, []button{{Text: candidate, Data: fmt.Sprintf("domain:pick:%d", n)}})
			}
			rows = append(rows, []button{{Text: "取消", Data: "nav:cancel"}})
			b.send(ctx, item.Message.Chat.ID, "检测到多个域名，请选择：", keyboard(rows))
		}
	case "/remove":
		b.remove(ctx, item.Message.Chat.ID, parts)
	case "/list":
		b.list(ctx, item.Message.Chat.ID)
	case "/sync":
		b.sync(ctx, item.Message.Chat.ID)
	case "/status":
		b.status(ctx, item.Message.Chat.ID)
	default:
		b.send(ctx, item.Message.Chat.ID, "无法识别这个操作，请点击消息中的按钮或发送 /help。", mainMenu())
	}
}

func (b *Bot) handlePendingText(ctx context.Context, chatID int64, text string) bool {
	b.mu.Lock()
	p := b.sessions[chatID]
	b.mu.Unlock()
	if p == nil || p.Mode == "" {
		return false
	}
	if p.AwaitingPattern {
		value, err := rules.ValidatePattern(p.AdvancedMatch, text)
		if err != nil {
			b.send(ctx, chatID, "匹配内容无效："+err.Error()+"\n请重新输入，或点击取消。", cancelMenu())
			return true
		}
		p.AwaitingPattern = false
		p.Domain = value
		p.Match = p.AdvancedMatch
		b.showAdvancedConfirmation(ctx, chatID, p)
		return true
	}
	if p.Domain != "" {
		return false
	}
	if strings.TrimSpace(text) == "" {
		b.send(ctx, chatID, "请输入域名，例如 example.com：", cancelMenu())
		return true
	}
	candidates, err := domain.Extract(text)
	if err != nil {
		if p.Mode == "remove" {
			value := strings.TrimSpace(text)
			b.clear(chatID)
			b.removeDomain(ctx, chatID, value)
			return true
		}
		b.send(ctx, chatID, "域名无效："+err.Error()+"\n请重新发送域名或 URL。", cancelMenu())
		return true
	}
	if len(candidates) > 1 {
		if len(candidates) > 8 {
			candidates = candidates[:8]
		}
		b.mu.Lock()
		p.Candidates = candidates
		b.mu.Unlock()
		rows := make([][]button, 0, len(candidates)+1)
		for n, candidate := range candidates {
			rows = append(rows, []button{{Text: candidate, Data: fmt.Sprintf("domain:pick:%d", n)}})
		}
		rows = append(rows, []button{{Text: "取消", Data: "nav:cancel"}})
		b.send(ctx, chatID, "检测到多个域名，请选择需要处理的目标：", keyboard(rows))
		return true
	}
	b.acceptDomain(ctx, chatID, p, candidates[0])
	return true
}

func (b *Bot) acceptDomain(ctx context.Context, chatID int64, p *pending, domainName string) {
	switch p.Mode {
	case "add":
		p.OriginalDomain = domainName
		p.Domain = domainName
		p.RootDomain = domain.RuleRoot(domainName)
		b.showMatchMenu(ctx, chatID, p)
	case "query":
		b.clear(chatID)
		b.query(ctx, chatID, domainName)
	case "remove":
		b.clear(chatID)
		b.removeDomain(ctx, chatID, domainName)
	}
}

func (b *Bot) showMatchMenu(ctx context.Context, chatID int64, p *pending) {
	text, menu := matchMenuContent(p)
	b.send(ctx, chatID, text, menu)
}

func matchMenuContent(p *pending) (string, *models.InlineKeyboardMarkup) {
	input := p.OriginalDomain
	if input == "" {
		input = p.Domain
		p.OriginalDomain = input
	}
	root := p.RootDomain
	if root == "" {
		root = domain.RuleRoot(input)
		p.RootDomain = root
	}
	var text strings.Builder
	fmt.Fprintf(&text, "请选择规则覆盖范围：\n\n输入域名：%s", input)
	if root != "" {
		fmt.Fprintf(&text, "\n安全主域名：%s", root)
	}
	fmt.Fprintf(&text, "\n\n🎯 仅当前域名\nDOMAIN,%s\n优点：范围最小，不会误伤其他子域名\n缺点：123.%s 不会命中", input, input)
	rows := [][]button{{{Text: "🎯 仅 " + input, Data: "match:exact"}}}
	if root == "" {
		text.WriteString("\n\n⚠️ 无法安全识别可注册主域名，因此已隐藏后缀范围，避免覆盖公共或共享后缀。")
	} else if root == input {
		fmt.Fprintf(&text, "\n\n🌐 整个主域名（推荐）\nDOMAIN-SUFFIX,%s\n优点：匹配主域名和所有子域名\n缺点：www、api、cdn 等都会使用同一动作", input)
		rows = append(rows, []button{{Text: "⭐ 整个 " + input, Data: "match:suffix"}})
	} else {
		fmt.Fprintf(&text, "\n\n🌿 当前域名及下级（推荐）\nDOMAIN-SUFFIX,%s\n优点：覆盖当前域名和所有下级域名\n缺点：不会覆盖同主域名下的其他分支", input)
		rows = append(rows, []button{{Text: "⭐ " + input + " + 下级", Data: "match:suffix"}})
		if root != "" {
			fmt.Fprintf(&text, "\n\n🌐 整个主域名\nDOMAIN-SUFFIX,%s\n优点：一次覆盖整个网站\n缺点：所有子域名都会使用同一动作", root)
			rows = append(rows, []button{{Text: "🌐 整个 " + root, Data: "match:root"}})
		}
	}
	rows = append(rows,
		[]button{{Text: "🧰 高级匹配", Data: "match:advanced"}},
		[]button{{Text: "取消", Data: "nav:cancel"}},
	)
	return text.String(), keyboard(rows)
}

func (b *Bot) showAdvancedMenu(ctx context.Context, chatID int64, p *pending) {
	text := "🧰 高级域名匹配\n\n" +
		"🔑 DOMAIN-KEYWORD\n包含关键词即命中；灵活但容易误匹配。\n\n" +
		"🌟 DOMAIN-WILDCARD\n使用 * 和 ?；比关键词可控，但 *.example.com 通常不匹配根域名。\n\n" +
		"🧩 DOMAIN-REGEX\n表达能力最强；最难维护且容易写错。\n\n" +
		"常规域名优先使用上一页的 DOMAIN 或 DOMAIN-SUFFIX。"
	b.send(ctx, chatID, text, keyboard([][]button{
		{{Text: "🔑 关键词", Data: "advanced:keyword"}},
		{{Text: "🌟 通配符", Data: "advanced:wildcard"}},
		{{Text: "🧩 正则表达式", Data: "advanced:regex"}},
		{{Text: "⬅️ 返回范围选择", Data: "advanced:back"}, {Text: "取消", Data: "nav:cancel"}},
	}))
}

func (b *Bot) showAdvancedOption(ctx context.Context, chatID int64, p *pending) {
	name, benefit, risk := advancedDescription(p.AdvancedMatch)
	text := fmt.Sprintf("%s\n\n建议值：\n%s\n\n优点：%s\n风险：%s\n\n你可以使用建议值，或者输入自定义内容。", name, p.AdvancedSuggest, benefit, risk)
	b.send(ctx, chatID, text, keyboard([][]button{
		{{Text: "✅ 使用建议", Data: "advanced:use"}, {Text: "✍️ 自定义输入", Data: "advanced:custom"}},
		{{Text: "⬅️ 返回高级匹配", Data: "match:advanced"}, {Text: "取消", Data: "nav:cancel"}},
	}))
}

func (b *Bot) showAdvancedConfirmation(ctx context.Context, chatID int64, p *pending) {
	name, _, risk := advancedDescription(p.Match)
	text := fmt.Sprintf("⚠️ 请确认高级规则\n\n类型：%s\n准备提交：%s\n动作：%s\n\n风险：%s", name, rules.Token(toRule(p, 0)), actionText(p.Action), risk)
	b.send(ctx, chatID, text, keyboard([][]button{
		{{Text: "确认提交", Data: "advanced:confirm"}, {Text: "取消", Data: "nav:cancel"}},
	}))
}

func (b *Bot) commitPending(ctx context.Context, chatID, userID int64, p *pending, force bool) {
	if !b.markBusy(chatID) {
		b.send(ctx, chatID, "⏳ 当前规则正在处理中，请勿重复点击。", homeMenu())
		return
	}
	verb := "正在检查冲突并提交"
	if force {
		verb = "正在移动规则并提交"
	}
	b.send(ctx, chatID, fmt.Sprintf("⏳ 已选择：%s\n准备提交：%s\n%s到%s，请稍候……", matchText(p.Match), rules.Token(toRule(p, userID)), verb, repoProviderText(b.cfg.RuleRepoProvider)), nil)
	result, err := b.service.AddRule(ctx, toRule(p, userID), force)
	if conflict, ok := err.(*app.ConflictError); ok {
		b.setBusy(chatID, false)
		b.send(ctx, chatID, fmt.Sprintf("发现同类型规则：\n%s,%s\n当前动作：%s\n目标动作：%s\n\n是否移动到新的动作？", rules.Token(conflict.Existing), strings.ToUpper(string(conflict.Existing.Action)), actionText(conflict.Existing.Action), actionText(p.Action)), keyboard([][]button{
			{{Text: "确认移动", Data: "confirm:yes"}, {Text: "取消", Data: "nav:cancel"}},
		}))
		return
	}
	b.clear(chatID)
	if err != nil {
		message := err.Error()
		if strings.Contains(message, "rule already exists") {
			message = "该规则已经存在，无需重复提交"
		}
		b.send(ctx, chatID, "提交失败："+message, homeMenu())
		return
	}
	var extra string
	if len(result.Related) > 0 {
		extra = fmt.Sprintf("\n\n⚠️ 已保留 %d 条相反动作的父子/重叠规则。生成的显式规则会按精确度排序，更具体的规则优先。", len(result.Related))
	}
	b.send(ctx, chatID, fmt.Sprintf("✅ 规则已提交\n\n动作：%s\n规则：%s\n覆盖：%s\ncommit：%s%s", actionText(p.Action), rules.Token(toRule(p, userID)), coverageText(p), short(result.Commit), extra), homeMenu())
}

func (b *Bot) startAddMode(ctx context.Context, chatID int64, action rules.Action) {
	b.mu.Lock()
	b.sessions[chatID] = &pending{Mode: "add", Action: action}
	b.mu.Unlock()
	actionText := "直连"
	if action == rules.Proxy {
		actionText = "代理"
	}
	b.send(ctx, chatID, fmt.Sprintf("将域名添加到“%s”规则，请发送域名或 URL：", actionText), cancelMenu())
}

func (b *Bot) startQueryMode(ctx context.Context, chatID int64) {
	b.mu.Lock()
	b.sessions[chatID] = &pending{Mode: "query"}
	b.mu.Unlock()
	b.send(ctx, chatID, "请输入要查询的域名或 URL，例如：example.com", cancelMenu())
}

func (b *Bot) startRemoveMode(ctx context.Context, chatID int64) {
	b.mu.Lock()
	b.sessions[chatID] = &pending{Mode: "remove"}
	b.mu.Unlock()
	b.send(ctx, chatID, "请输入要删除的域名或 URL：", cancelMenu())
}

func (b *Bot) addStart(ctx context.Context, chatID int64, parts []string) {
	if len(parts) < 2 {
		b.send(ctx, chatID, "用法：/add example.com", homeMenu())
		return
	}
	domainName, err := domain.Normalize(strings.Join(parts[1:], " "))
	if err != nil {
		b.send(ctx, chatID, "域名无效："+err.Error(), homeMenu())
		return
	}
	b.mu.Lock()
	b.sessions[chatID] = &pending{Mode: "add", OriginalDomain: domainName, Domain: domainName, RootDomain: domain.RuleRoot(domainName)}
	b.mu.Unlock()
	b.send(ctx, chatID, "请选择规则动作：", keyboard([][]button{
		{{Text: "直连", Data: "route:direct"}, {Text: "代理", Data: "route:proxy"}},
		{{Text: "取消", Data: "nav:cancel"}},
	}))
}

func (b *Bot) handleCallback(ctx context.Context, query *models.CallbackQuery) {
	message := callbackMessage(query)
	if query == nil || message == nil {
		return
	}
	if !b.allowed(query.From.ID, message.Chat.Type == models.ChatTypePrivate) {
		log.Printf("telegram callback rejected from=%d chat_type=%s", query.From.ID, message.Chat.Type)
		return
	}
	b.answer(ctx, query.ID)
	chatID := message.Chat.ID

	switch query.Data {
	case "menu:query":
		b.startQueryMode(ctx, chatID)
		return
	case "menu:add:direct":
		b.startAddMode(ctx, chatID, rules.Direct)
		return
	case "menu:add:proxy":
		b.startAddMode(ctx, chatID, rules.Proxy)
		return
	case "menu:remove":
		b.startRemoveMode(ctx, chatID)
		return
	case "menu:list":
		b.clear(chatID)
		b.list(ctx, chatID)
		return
	case "menu:status":
		b.clear(chatID)
		b.status(ctx, chatID)
		return
	case "menu:repo":
		b.clear(chatID)
		b.repo(ctx, chatID)
		return
	case "menu:help":
		b.clear(chatID)
		b.send(ctx, chatID, b.helpText(), homeMenu())
		return
	case "nav:home":
		b.clear(chatID)
		b.send(ctx, chatID, b.welcomeText(), mainMenu())
		return
	case "nav:cancel", "cancel", "confirm:no":
		b.clear(chatID)
		b.send(ctx, chatID, "已取消当前操作。", homeMenu())
		return
	}

	b.mu.Lock()
	pendingRule := b.sessions[chatID]
	b.mu.Unlock()
	if pendingRule == nil {
		b.send(ctx, chatID, "操作已过期，请返回主菜单重新开始。", homeMenu())
		return
	}
	if strings.HasPrefix(query.Data, "domain:pick:") {
		var n int
		if _, err := fmt.Sscanf(query.Data, "domain:pick:%d", &n); err != nil || n < 0 || n >= len(pendingRule.Candidates) {
			b.send(ctx, chatID, "域名候选已过期，请重新发送。", cancelMenu())
			return
		}
		domainName := pendingRule.Candidates[n]
		pendingRule.Candidates = nil
		b.acceptDomain(ctx, chatID, pendingRule, domainName)
		return
	}

	switch query.Data {
	case "route:direct":
		pendingRule.Action = rules.Direct
	case "route:proxy":
		pendingRule.Action = rules.Proxy
	case "match:exact":
		pendingRule.Domain = pendingRule.OriginalDomain
		pendingRule.Match = rules.Exact
	case "match:suffix":
		pendingRule.Domain = pendingRule.OriginalDomain
		pendingRule.Match = rules.Suffix
	case "match:root":
		if pendingRule.RootDomain == "" {
			b.send(ctx, chatID, "无法安全识别主域名，请选择其他范围。", cancelMenu())
			return
		}
		pendingRule.Domain = pendingRule.RootDomain
		pendingRule.Match = rules.Suffix
	case "match:advanced":
		b.showAdvancedMenu(ctx, chatID, pendingRule)
		return
	case "advanced:keyword", "advanced:wildcard", "advanced:regex":
		pendingRule.AdvancedMatch = rules.Match(strings.TrimPrefix(query.Data, "advanced:"))
		pendingRule.AdvancedSuggest = advancedSuggestion(pendingRule.AdvancedMatch, pendingRule.OriginalDomain, pendingRule.RootDomain)
		b.showAdvancedOption(ctx, chatID, pendingRule)
		return
	case "advanced:back":
		b.showMatchMenu(ctx, chatID, pendingRule)
		return
	case "advanced:use":
		value, err := rules.ValidatePattern(pendingRule.AdvancedMatch, pendingRule.AdvancedSuggest)
		if err != nil {
			b.send(ctx, chatID, "建议规则生成失败："+err.Error(), cancelMenu())
			return
		}
		pendingRule.Domain = value
		pendingRule.Match = pendingRule.AdvancedMatch
		b.showAdvancedConfirmation(ctx, chatID, pendingRule)
		return
	case "advanced:custom":
		pendingRule.AwaitingPattern = true
		b.send(ctx, chatID, advancedInputPrompt(pendingRule.AdvancedMatch), cancelMenu())
		return
	case "advanced:confirm":
		b.commitPending(ctx, chatID, query.From.ID, pendingRule, false)
		return
	case "override:direct":
		pendingRule.Mode = "add"
		pendingRule.Action = rules.Direct
		pendingRule.Match = ""
		pendingRule.OriginalDomain = pendingRule.Domain
		pendingRule.RootDomain = domain.RuleRoot(pendingRule.Domain)
	case "override:proxy":
		pendingRule.Mode = "add"
		pendingRule.Action = rules.Proxy
		pendingRule.Match = ""
		pendingRule.OriginalDomain = pendingRule.Domain
		pendingRule.RootDomain = domain.RuleRoot(pendingRule.Domain)
	case "suggest:direct":
		pendingRule.Mode = "add"
		pendingRule.Action = rules.Direct
		pendingRule.Match = ""
		pendingRule.OriginalDomain = pendingRule.Domain
		pendingRule.RootDomain = domain.RuleRoot(pendingRule.Domain)
	case "suggest:proxy":
		pendingRule.Mode = "add"
		pendingRule.Action = rules.Proxy
		pendingRule.Match = ""
		pendingRule.OriginalDomain = pendingRule.Domain
		pendingRule.RootDomain = domain.RuleRoot(pendingRule.Domain)
	case "confirm:yes":
		b.commitPending(ctx, chatID, query.From.ID, pendingRule, true)
		return
	default:
		log.Printf("telegram callback ignored: unknown data=%q", query.Data)
		b.send(ctx, chatID, "无法识别这个按钮，请返回主菜单。", homeMenu())
		return
	}

	if pendingRule.Action != "" && pendingRule.Match == "" {
		b.showMatchMenu(ctx, chatID, pendingRule)
		return
	}
	if pendingRule.Action != "" && pendingRule.Match != "" {
		b.commitPending(ctx, chatID, query.From.ID, pendingRule, false)
	}
}

func toRule(p *pending, userID int64) rules.Rule {
	return rules.Rule{Domain: p.Domain, Match: p.Match, Action: p.Action, CreatedBy: userID, CreatedAt: time.Now().UTC()}
}

func (b *Bot) remove(ctx context.Context, chatID int64, parts []string) {
	if len(parts) < 2 {
		b.send(ctx, chatID, "用法：/remove example.com", homeMenu())
		return
	}
	value := strings.Join(parts[1:], " ")
	domainName, err := domain.Normalize(value)
	if err != nil {
		domainName = strings.TrimSpace(value)
	}
	b.removeDomain(ctx, chatID, domainName)
}

func (b *Bot) removeDomain(ctx context.Context, chatID int64, domainName string) {
	result, err := b.service.RemoveRule(ctx, domainName, nil)
	if err != nil {
		b.send(ctx, chatID, "删除失败："+err.Error(), homeMenu())
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("已删除 %d 条规则，commit=%s", result.Changed, short(result.Commit)), homeMenu())
}

func (b *Bot) query(ctx context.Context, chatID int64, domainName string) {
	result, err := b.service.Query(ctx, domainName)
	if err != nil {
		b.send(ctx, chatID, "查询失败："+err.Error(), homeMenu())
		return
	}
	personal := "未找到"
	if len(result.Personal) > 0 {
		var values []string
		for _, r := range result.Personal {
			values = append(values, fmt.Sprintf("%s/%s (%s)", actionText(r.Action), matchText(r.Match), r.Domain))
		}
		personal = strings.Join(values, "；")
	}
	var upstreamText []string
	var geositeText, gfwText []string
	hasDirect, hasProxy := false, false
	const maxDisplayedMatches = 20
	for _, match := range result.Upstream {
		if match.Action == ruleindex.GeoSiteCN {
			geositeText = append(geositeText, fmt.Sprintf("命中 · %s · %s · 来源 %s", match.Kind, match.Pattern, match.Source))
			continue
		}
		if match.Action == ruleindex.GeoSiteGFW {
			gfwText = append(gfwText, fmt.Sprintf("命中 · %s · %s · 来源 %s", match.Kind, match.Pattern, match.Source))
			hasProxy = true
			continue
		}
		if match.Action == ruleindex.Direct {
			hasDirect = true
		}
		if match.Action == ruleindex.Proxy {
			hasProxy = true
		}
		upstreamText = append(upstreamText, fmt.Sprintf("%s · %s · %s · 来源 %s", indexActionText(match.Action), match.Kind, match.Pattern, match.Source))
	}
	if len(upstreamText) > maxDisplayedMatches {
		rest := len(upstreamText) - maxDisplayedMatches
		upstreamText = append(upstreamText[:maxDisplayedMatches], fmt.Sprintf("… 其余 %d 条命中未显示", rest))
	}
	if len(geositeText) > maxDisplayedMatches {
		rest := len(geositeText) - maxDisplayedMatches
		geositeText = append(geositeText[:maxDisplayedMatches], fmt.Sprintf("… 其余 %d 条命中未显示", rest))
	}
	if len(upstreamText) == 0 {
		upstreamText = []string{"未找到"}
	}
	if len(geositeText) == 0 {
		geositeText = []string{"未找到"}
	}
	if len(gfwText) == 0 {
		gfwText = []string{"未找到"}
	}
	chinaSignal := fmt.Sprintf("未检测到（已检查 %d 个真实地址）", result.Network.ChinaChecked)
	if result.Network.GeoError != "" {
		chinaSignal = "检测失败（" + result.Network.GeoError + "）"
	} else if result.Network.China {
		chinaSignal = fmt.Sprintf("检测到（已检查 %d 个真实地址）", result.Network.ChinaChecked)
	} else if len(result.Network.A)+len(result.Network.AAAA) == 0 {
		chinaSignal = "无法判断（没有取得真实公网 IP）"
	}
	dnsText := formatDNSReport(result.Network)
	geoText := formatGeoIPReport(result.Network)
	suggestion, suggestedAction := geoSuggestion(result.Network)
	root := result.Network.Registrable
	if root == "" {
		root = "未识别"
	}
	text := fmt.Sprintf("🔍 查询域名：%s\n👤 个人规则：%s\n📚 Aethersailor：%s\n🇨🇳 GEOSITE:CN：%s\n🧱 GEOSITE:GFW：%s\n\n🌐 DNS 解析：%s\n可注册域名：%s\n\n📡 中国大陆信号：%s", domainName, personal, strings.Join(upstreamText, "\n  "), strings.Join(geositeText, "；"), strings.Join(gfwText, "；"), dnsText, root, chinaSignal)
	if geoText != "" {
		text += "\n\n" + geoText
	}
	if suggestion != "" {
		text += "\n\n" + suggestion
	}
	rows := [][]button{}
	if hasProxy {
		rows = append(rows, []button{{Text: "🎯 覆写为个人直连", Data: "override:direct"}})
	}
	if hasDirect {
		rows = append(rows, []button{{Text: "🚀 覆写为个人代理", Data: "override:proxy"}})
	}
	rows = append(rows, []button{{Text: "🏠 返回主菜单", Data: "nav:home"}})
	if hasDirect || hasProxy {
		b.mu.Lock()
		b.sessions[chatID] = &pending{Mode: "query_override", OriginalDomain: domainName, Domain: domainName, RootDomain: domain.RuleRoot(domainName)}
		b.mu.Unlock()
	}
	if suggestedAction != "" {
		label := "💡 建议添加代理"
		if suggestedAction == rules.Direct {
			label = "💡 建议添加直连"
		}
		rows = append(rows, []button{{Text: label, Data: "suggest:" + string(suggestedAction)}})
		if !hasDirect && !hasProxy {
			b.mu.Lock()
			b.sessions[chatID] = &pending{Mode: "query_override", OriginalDomain: domainName, Domain: domainName, RootDomain: domain.RuleRoot(domainName)}
			b.mu.Unlock()
		}
	}
	b.send(ctx, chatID, text, keyboard(rows))
}

func (b *Bot) list(ctx context.Context, chatID int64) {
	store, err := b.service.LoadStore(ctx)
	if err != nil {
		b.send(ctx, chatID, "读取规则失败："+err.Error(), homeMenu())
		return
	}
	if len(store.Rules) == 0 {
		b.send(ctx, chatID, "当前没有个人规则。", homeMenu())
		return
	}
	var out strings.Builder
	fmt.Fprintf(&out, "个人规则共 %d 条：\n", len(store.Rules))
	ordered := rules.Ordered(store.Rules)
	for i, r := range ordered {
		if i >= 80 {
			fmt.Fprintf(&out, "\n… 其余 %d 条未显示", len(ordered)-i)
			break
		}
		fmt.Fprintf(&out, "%d. %s → %s\n", i+1, rules.Token(r), actionText(r.Action))
	}
	b.send(ctx, chatID, out.String(), homeMenu())
}

func (b *Bot) sync(ctx context.Context, chatID int64) {
	if !b.service.IndexEnabled() && !b.service.SyncEnabled() {
		b.send(ctx, chatID, "本地查询索引和上游镜像都已关闭。", homeMenu())
		return
	}
	b.send(ctx, chatID, "正在同步 Aethersailor、GEOSITE:CN 与 GEOSITE:GFW 本地索引，请稍候…", nil)
	result, err := b.service.Sync(ctx)
	if err != nil {
		b.send(ctx, chatID, "同步失败："+err.Error(), homeMenu())
		return
	}
	msg := "同步完成：本地索引无变化"
	if result.IndexChanged {
		msg = "同步完成：本地索引已更新"
	}
	if result.Commit != "" {
		msg += fmt.Sprintf("\n镜像 %d 个文件，commit=%s", result.Changed, short(result.Commit))
	}
	b.send(ctx, chatID, msg, homeMenu())
}

func (b *Bot) status(ctx context.Context, chatID int64) {
	store, err := b.service.LoadStore(ctx)
	if err != nil {
		b.send(ctx, chatID, "状态读取失败："+err.Error(), homeMenu())
		return
	}
	idx := b.service.IndexStatus()
	dns := b.service.LookupStatus()
	updated := "从未"
	if !idx.UpdatedAt.IsZero() {
		updated = idx.UpdatedAt.In(b.cfg.Location).Format("2006-01-02 15:04:05")
	}
	lastError := "无"
	if idx.LastError != "" {
		lastError = idx.LastError
	}
	dohError := "无"
	if dns.LastError != "" {
		dohError = dns.LastError
	}
	provider := dns.LastProvider
	if provider == "" {
		provider = "尚未使用"
	}
	b.send(ctx, chatID, fmt.Sprintf("ClashRulePilot 运行状态\n发布仓库：%s (%s)\n个人规则：%d 条\n磁盘查询数据库：%t / 已加载=%t\n落地规则文件：%d 个（只保留远端当前版本）\n索引规则：直连 %d · 代理 %d · 分类 %d · GEOSITE:CN %d · GEOSITE:GFW %d\n索引更新时间：%s\n索引异常：%s\n公网 DoH：启用=%t · 缓存=%d · 最近=%s\nDoH 异常：%s\n上游公开镜像：%t", b.cfg.RuleRepoProject, b.cfg.RuleRepoProvider, len(store.Rules), idx.Enabled, idx.Loaded, idx.Sources, idx.Direct, idx.Proxy, idx.Category, idx.GeoSite, idx.GFW, updated, lastError, dns.Enabled, dns.CacheEntries, provider, dohError, b.service.SyncEnabled()), homeMenu())
}

func (b *Bot) helpText() string {
	return "ClashRulePilot 使用帮助\n\n支持纯域名、URL、域名:端口和完整 OpenClash/Mihomo 日志。Fake-IP 模式下会使用公网 DoH 获取真实 IP。\n\n域名匹配：\n• DOMAIN：仅完整域名，最安全，但不含子域名。\n• DOMAIN-SUFFIX：域名及所有下级，最适合网站/CDN，但主域名范围可能过大。\n• DOMAIN-KEYWORD：包含关键词即命中，灵活但容易误伤。\n• DOMAIN-WILDCARD：支持 * 和 ?，可控但规则更难理解。\n• DOMAIN-REGEX：能力最强，但最难维护且匹配成本最高。\n• GEOSITE：维护好的域名分类，依赖数据库更新。\n• RULE-SET：批量远程规则，存在更新延迟。\n\n命令兼容：/query、/add、/remove、/list、/sync、/status、/help"
}

func (b *Bot) welcomeText() string {
	return "ClashRulePilot\n\n请选择要执行的操作："
}

func (b *Bot) repo(ctx context.Context, chatID int64) {
	b.send(ctx, chatID, fmt.Sprintf("规则仓库\n%s\n\n个人覆写：\n%s", b.service.RepoWebURL(), b.service.RepoRawURL("openclash/personal-overwrite.ini")), homeMenu())
}

func (b *Bot) clear(id int64) { b.mu.Lock(); delete(b.sessions, id); b.mu.Unlock() }
func (b *Bot) markBusy(id int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.sessions[id]
	if p == nil || p.Busy {
		return false
	}
	p.Busy = true
	return true
}
func (b *Bot) setBusy(id int64, busy bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p := b.sessions[id]; p != nil {
		p.Busy = busy
	}
}
func (b *Bot) answer(ctx context.Context, id string) {
	if _, err := b.api.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{CallbackQueryID: id}); err != nil {
		log.Printf("telegram answer callback: %v", err)
	}
}
func keyboard(rows [][]button) *models.InlineKeyboardMarkup {
	result := make([][]models.InlineKeyboardButton, len(rows))
	for i, row := range rows {
		result[i] = make([]models.InlineKeyboardButton, len(row))
		for j, item := range row {
			result[i][j] = models.InlineKeyboardButton{Text: item.Text, CallbackData: item.Data}
		}
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: result}
}
func mainMenu() *models.InlineKeyboardMarkup {
	return keyboard([][]button{
		{{Text: "🔍 查询域名", Data: "menu:query"}, {Text: "➕ 添加直连", Data: "menu:add:direct"}},
		{{Text: "🚀 添加代理", Data: "menu:add:proxy"}, {Text: "➖ 删除规则", Data: "menu:remove"}},
		{{Text: "📋 规则列表", Data: "menu:list"}, {Text: "📊 运行状态", Data: "menu:status"}},
		{{Text: "🔗 规则仓库", Data: "menu:repo"}, {Text: "ℹ️ 使用帮助", Data: "menu:help"}},
	})
}
func homeMenu() *models.InlineKeyboardMarkup {
	return keyboard([][]button{{{Text: "🏠 返回主菜单", Data: "nav:home"}}})
}
func cancelMenu() *models.InlineKeyboardMarkup {
	return keyboard([][]button{{{Text: "取消", Data: "nav:cancel"}, {Text: "🏠 主菜单", Data: "nav:home"}}})
}
func (b *Bot) send(ctx context.Context, chatID int64, text string, kb models.ReplyMarkup) {
	_, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text, ReplyMarkup: kb})
	if err != nil {
		log.Printf("telegram send: %v", err)
		return
	}
	log.Printf("telegram message sent chat=%d", chatID)
}

func callbackMessage(query *models.CallbackQuery) *models.Message {
	if query == nil || query.Message.Message == nil {
		return nil
	}
	return query.Message.Message
}
func short(s string) string {
	if s == "" {
		return "无变化"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func actionText(a rules.Action) string {
	if a == rules.Direct {
		return "直连"
	}
	return "代理"
}
func matchText(m rules.Match) string {
	switch m {
	case rules.Exact:
		return "仅当前域名"
	case rules.Suffix:
		return "域名及下级"
	case rules.Keyword:
		return "关键词"
	case rules.Wildcard:
		return "通配符"
	case rules.Regex:
		return "正则表达式"
	default:
		return "未知"
	}
}
func indexActionText(a ruleindex.Action) string {
	switch a {
	case ruleindex.Direct:
		return "直连"
	case ruleindex.Proxy:
		return "代理"
	case ruleindex.Category:
		return "分类"
	default:
		return "未知"
	}
}

func advancedSuggestion(match rules.Match, input, root string) string {
	if root == "" {
		root = input
	}
	switch match {
	case rules.Keyword:
		parts := strings.Split(input, ".")
		candidate := parts[0]
		common := map[string]bool{"www": true, "api": true, "cdn": true, "static": true, "img": true, "assets": true, "gw": true, "gateway": true}
		if common[candidate] {
			candidate = strings.Split(root, ".")[0]
		}
		return candidate
	case rules.Wildcard:
		return "*." + root
	case rules.Regex:
		return `(^|\.)` + regexp.QuoteMeta(root) + `$`
	default:
		return input
	}
}

func advancedDescription(match rules.Match) (name, benefit, risk string) {
	switch match {
	case rules.Keyword:
		return "🔑 DOMAIN-KEYWORD", "域名结构变化时仍可按固定标识命中", "任何包含该字符串的无关域名也可能被匹配"
	case rules.Wildcard:
		return "🌟 DOMAIN-WILDCARD", "可以使用 * 和 ? 描述固定域名格式", "*.example.com 通常只覆盖子域名，不包含 example.com"
	case rules.Regex:
		return "🧩 DOMAIN-REGEX", "可以表达复杂的域名模式", "最难维护；表达式过宽会误匹配，错误表达式无法提交"
	default:
		return "高级匹配", "", ""
	}
}

func advancedInputPrompt(match rules.Match) string {
	switch match {
	case rules.Keyword:
		return "请输入关键词，例如：oops\n只要域名中包含该文字就会命中。"
	case rules.Wildcard:
		return "请输入通配符，例如：*.oops.asia\n支持 * 和 ?。"
	case rules.Regex:
		return "请输入正则表达式，例如：(^|\\.)oops\\.asia$\n使用 Go/Mihomo 兼容的正则语法。"
	default:
		return "请输入匹配内容："
	}
}

func coverageText(p *pending) string {
	switch p.Match {
	case rules.Exact:
		return "仅 " + p.Domain
	case rules.Suffix:
		return p.Domain + " 以及所有下级域名"
	case rules.Keyword:
		return "所有包含 “" + p.Domain + "” 的域名"
	case rules.Wildcard:
		return "所有符合通配符 “" + p.Domain + "” 的域名"
	case rules.Regex:
		return "所有符合正则表达式的域名"
	default:
		return p.Domain
	}
}

func repoProviderText(provider string) string {
	if strings.EqualFold(provider, "gitlab") {
		return " GitLab"
	}
	return " GitHub"
}

func formatDNSReport(report lookup.Report) string {
	var out strings.Builder
	switch report.DNSSource {
	case "doh":
		fmt.Fprintf(&out, "公网 DoH · %s", report.DoHProvider)
		if report.DoHCacheHit {
			out.WriteString("（缓存）")
		}
	case "local":
		out.WriteString("本地 DNS")
	default:
		out.WriteString("未取得真实结果")
	}
	if len(report.FakeIP) > 0 {
		fmt.Fprintf(&out, "\n本地 DNS：Fake-IP %d", len(report.FakeIP))
	} else if report.DNSError != "" {
		out.WriteString("\n本地 DNS：解析失败")
	} else {
		fmt.Fprintf(&out, "\n本地 DNS：A %d · AAAA %d", len(report.LocalA), len(report.LocalAAAA))
	}
	fmt.Fprintf(&out, "\n真实结果：A %d · AAAA %d", len(report.A), len(report.AAAA))
	addresses := append(append([]string{}, report.A...), report.AAAA...)
	if len(addresses) > 0 {
		display := addresses
		if len(display) > 4 {
			display = display[:4]
		}
		fmt.Fprintf(&out, "\n真实 IP：%s", strings.Join(display, "、"))
		if len(addresses) > len(display) {
			fmt.Fprintf(&out, "（另有 %d 个）", len(addresses)-len(display))
		}
	}
	if report.DoHError != "" {
		fmt.Fprintf(&out, "\nDoH：失败（%s）", report.DoHError)
	}
	return out.String()
}

func formatGeoIPReport(report lookup.Report) string {
	if len(report.GeoIPs) == 0 {
		return "📍 IP 地域解析：未取得"
	}
	var out strings.Builder
	out.WriteString("📍 IP 地域解析：")
	shown := 0
	for _, info := range report.GeoIPs {
		if shown >= 8 {
			break
		}
		shown++
		out.WriteString("\n  ")
		if info.Error != "" {
			fmt.Fprintf(&out, "%s → 检测失败（%s）", info.IP, info.Error)
			continue
		}
		place := geoPlace(info)
		if info.China {
			place = "🇨🇳 " + place
		} else {
			place = "🌐 " + place
		}
		fmt.Fprintf(&out, "%s → %s", info.IP, place)
	}
	if len(report.GeoIPs) > shown {
		fmt.Fprintf(&out, "\n  … 其余 %d 个地址未显示", len(report.GeoIPs)-shown)
	}
	return out.String()
}

func geoPlace(info lookup.GeoIPInfo) string {
	parts := make([]string, 0, 5)
	if info.Country != "" {
		parts = append(parts, info.Country)
	} else if info.CountryCode != "" {
		parts = append(parts, info.CountryCode)
	}
	if info.Region != "" {
		parts = append(parts, info.Region)
	}
	if info.City != "" {
		parts = append(parts, info.City)
	}
	if info.ISP != "" {
		parts = append(parts, "ISP "+info.ISP)
	} else if info.Org != "" {
		parts = append(parts, "组织 "+info.Org)
	}
	if info.ASN != "" {
		asn := info.ASN
		if !strings.HasPrefix(strings.ToUpper(asn), "AS") {
			asn = "AS" + asn
		}
		parts = append(parts, asn)
	}
	if len(parts) == 0 {
		return "未知地域"
	}
	return strings.Join(parts, " · ")
}

func geoSuggestion(report lookup.Report) (string, rules.Action) {
	if len(report.GeoIPs) == 0 || report.ChinaChecked == 0 {
		return "💡 建议：暂时无法根据 IP 地域判断直连或代理。", ""
	}
	china, foreign := 0, 0
	for _, info := range report.GeoIPs {
		if info.Error != "" {
			continue
		}
		if info.China {
			china++
		} else {
			foreign++
		}
	}
	switch {
	case foreign > 0 && china == 0:
		return "💡 建议：真实 IP 均在中国大陆以外，通常建议添加到「代理」规则。CDN 位置可能变化，请结合实际访问情况确认。", rules.Proxy
	case china > 0 && foreign == 0:
		return "💡 建议：中国大陆真实 IP，通常建议添加到「直连」规则。若服务仍不可达，再改为代理。", rules.Direct
	default:
		return "💡 建议：该域名同时解析到中国大陆和境外地址，地域会随 CDN 变化，建议优先使用「代理」规则。", rules.Proxy
	}
}
