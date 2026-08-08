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
	"github.com/biter777/countries"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

type Bot struct {
	service       *app.Service
	cfg           config.Config
	api           *tgbot.Bot
	sessions      map[int64]*pending
	seenCallbacks map[string]time.Time
	chatLocks     sync.Map
	mu            sync.Mutex
}

type pageSnapshot struct {
	Text   string
	Markup models.ReplyMarkup
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
	DeleteRules     []rules.Rule
	QueryResult     *app.QueryResult
	UserID          int64
	ActiveMessageID int
	CurrentPage     *pageSnapshot
	PageStack       []pageSnapshot
}

type button struct {
	Text string
	Data string
}

func New(service *app.Service, cfg config.Config) (*Bot, error) {
	b := &Bot{
		service:       service,
		cfg:           cfg,
		sessions:      make(map[int64]*pending),
		seenCallbacks: make(map[string]time.Time),
	}
	api, err := tgbot.New(cfg.TelegramToken,
		tgbot.WithDefaultHandler(b.handle),
		tgbot.WithErrorsHandler(func(err error) { log.Printf("telegram library: %v", err) }),
		tgbot.WithCheckInitTimeout(10*time.Second),
		tgbot.WithAllowedUpdates(tgbot.AllowedUpdates{"message", "callback_query"}),
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
	unlock := b.lockChat(item.Message.Chat.ID)
	defer unlock()
	text := strings.TrimSpace(item.Message.Text)
	b.bindSessionOwner(item.Message.Chat.ID, item.Message.From.ID)
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
			b.replaceSession(item.Message.Chat.ID, &pending{Mode: "query", Candidates: candidates, UserID: item.Message.From.ID})
			rows := make([][]button, 0, len(candidates)+1)
			for n, candidate := range candidates {
				if n >= 8 {
					break
				}
				rows = append(rows, []button{{Text: candidate, Data: fmt.Sprintf("domain:pick:%d", n)}})
			}
			rows = append(rows, []button{{Text: "✖️ 取消", Data: "nav:cancel"}})
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
		rows = append(rows, []button{{Text: "✖️ 取消", Data: "nav:cancel"}})
		b.send(ctx, chatID, "检测到多个域名，请选择需要处理的目标：", keyboard(rows))
		return true
	}
	b.acceptDomain(ctx, chatID, p, candidates[0])
	return true
}

func (b *Bot) acceptDomain(ctx context.Context, chatID int64, p *pending, domainName string) {
	b.acceptDomainTarget(ctx, chatID, p, domainName, nil)
}

func (b *Bot) acceptDomainTarget(ctx context.Context, chatID int64, p *pending, domainName string, target *models.Message) {
	switch p.Mode {
	case "add":
		p.OriginalDomain = domainName
		p.Domain = domainName
		p.RootDomain = domain.RuleRoot(domainName)
		b.showMatchMenuTarget(ctx, chatID, p, target)
	case "query":
		b.query(ctx, chatID, domainName)
	case "remove":
		b.removeDomain(ctx, chatID, domainName)
	}
}

func (b *Bot) showMatchMenu(ctx context.Context, chatID int64, p *pending) {
	b.showMatchMenuTarget(ctx, chatID, p, nil)
}

func (b *Bot) showMatchMenuTarget(ctx context.Context, chatID int64, p *pending, target *models.Message) {
	text, menu := matchMenuContent(p)
	b.sendTarget(ctx, chatID, target, text, menu)
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
	rows := [][]button{{{Text: "🎯 仅当前域名", Data: "match:exact"}}}
	if root == "" {
		text.WriteString("\n\n⚠️ 无法安全识别可注册主域名，因此已隐藏后缀范围，避免覆盖公共或共享后缀。")
	} else if root == input {
		fmt.Fprintf(&text, "\n\n🌐 整个主域名（推荐）\nDOMAIN-SUFFIX,%s\n优点：匹配主域名和所有子域名\n缺点：www、api、cdn 等都会使用同一动作", input)
		rows = append(rows, []button{{Text: "🌐 整个主域名", Data: "match:suffix"}})
	} else {
		fmt.Fprintf(&text, "\n\n🌿 当前域名及下级（推荐）\nDOMAIN-SUFFIX,%s\n优点：覆盖当前域名和所有下级域名\n缺点：不会覆盖同主域名下的其他分支", input)
		rows = append(rows, []button{{Text: "🌿 当前域名及下级", Data: "match:suffix"}})
		if root != "" {
			fmt.Fprintf(&text, "\n\n🌐 整个主域名\nDOMAIN-SUFFIX,%s\n优点：一次覆盖整个网站\n缺点：所有子域名都会使用同一动作", root)
			rows = append(rows, []button{{Text: "🌐 整个主域名", Data: "match:root"}})
		}
	}
	rows = append(rows,
		[]button{{Text: "🧰 高级匹配", Data: "match:advanced"}},
		[]button{{Text: "✖️ 取消", Data: "nav:cancel"}},
	)
	return text.String(), keyboard(rows)
}

func (b *Bot) showAdvancedMenu(ctx context.Context, chatID int64, p *pending) {
	b.showAdvancedMenuTarget(ctx, chatID, p, nil)
}

func (b *Bot) showAdvancedMenuTarget(ctx context.Context, chatID int64, p *pending, target *models.Message) {
	text := "🧰 高级域名匹配\n\n" +
		"🔑 DOMAIN-KEYWORD\n包含关键词即命中；灵活但容易误匹配。\n\n" +
		"✳️ DOMAIN-WILDCARD\n使用 * 和 ?；比关键词可控，但 *.example.com 通常不匹配根域名。\n\n" +
		"🧩 DOMAIN-REGEX\n表达能力最强；最难维护且容易写错。\n\n" +
		"常规域名优先使用上一页的 DOMAIN 或 DOMAIN-SUFFIX。"
	b.sendTarget(ctx, chatID, target, text, keyboard([][]button{
		{{Text: "🔑 关键词", Data: "advanced:keyword"}},
		{{Text: "✳️ 通配符", Data: "advanced:wildcard"}},
		{{Text: "🧩 正则表达式", Data: "advanced:regex"}},
		{{Text: "↩️ 返回范围选择", Data: "advanced:back"}, {Text: "✖️ 取消", Data: "nav:cancel"}},
	}))
}

func (b *Bot) showAdvancedOption(ctx context.Context, chatID int64, p *pending) {
	b.showAdvancedOptionTarget(ctx, chatID, p, nil)
}

func (b *Bot) showAdvancedOptionTarget(ctx context.Context, chatID int64, p *pending, target *models.Message) {
	name, benefit, risk := advancedDescription(p.AdvancedMatch)
	text := fmt.Sprintf("%s\n\n建议值：\n%s\n\n优点：%s\n风险：%s\n\n你可以使用建议值，或者输入自定义内容。", name, p.AdvancedSuggest, benefit, risk)
	b.sendTarget(ctx, chatID, target, text, keyboard([][]button{
		{{Text: "✅ 使用建议", Data: "advanced:use"}, {Text: "✍️ 自定义输入", Data: "advanced:custom"}},
		{{Text: "↩️ 返回高级匹配", Data: "match:advanced"}, {Text: "✖️ 取消", Data: "nav:cancel"}},
	}))
}

func (b *Bot) showAdvancedConfirmation(ctx context.Context, chatID int64, p *pending) {
	b.showAdvancedConfirmationTarget(ctx, chatID, p, nil)
}

func (b *Bot) showAdvancedConfirmationTarget(ctx context.Context, chatID int64, p *pending, target *models.Message) {
	name, _, risk := advancedDescription(p.Match)
	text := fmt.Sprintf("⚠️ 请确认高级规则\n\n类型：%s\n准备提交：%s\n动作：%s\n\n风险：%s", name, rules.Token(toRule(p, 0)), actionText(p.Action), risk)
	b.sendTarget(ctx, chatID, target, text, keyboard([][]button{
		{{Text: "✅ 确认提交", Data: "advanced:confirm"}, {Text: "✖️ 取消", Data: "nav:cancel"}},
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
		b.send(ctx, chatID, fmt.Sprintf("⚠️ 更改域名分组确认（第 2 步）\n\n规则：%s\n当前分组：%s\n目标分组：%s\n\n确认后会移动现有规则并生成新的 Git commit。OpenClash 下次更新远程覆写后将使用新分组。", rules.Token(conflict.Existing), actionText(conflict.Existing.Action), actionText(p.Action)), keyboard([][]button{
			{{Text: "🔄 确认更改分组", Data: "confirm:yes"}, {Text: "✖️ 取消", Data: "nav:cancel"}},
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
	b.startAddModeTarget(ctx, chatID, action, nil)
}

func (b *Bot) startAddModeTarget(ctx context.Context, chatID int64, action rules.Action, target *models.Message) {
	b.replaceSession(chatID, &pending{Mode: "add", Action: action})
	actionText := "直连"
	if action == rules.Proxy {
		actionText = "代理"
	}
	b.sendTarget(ctx, chatID, target, fmt.Sprintf("将域名添加到“%s”规则，请发送域名或 URL：", actionText), cancelMenu())
}

func (b *Bot) startQueryMode(ctx context.Context, chatID int64) {
	b.startQueryModeTarget(ctx, chatID, nil)
}

func (b *Bot) startQueryModeTarget(ctx context.Context, chatID int64, target *models.Message) {
	b.replaceSession(chatID, &pending{Mode: "query"})
	b.sendTarget(ctx, chatID, target, "请输入要查询的域名或 URL，例如：example.com", cancelMenu())
}

func (b *Bot) startRemoveMode(ctx context.Context, chatID int64) {
	b.startRemoveModeTarget(ctx, chatID, nil)
}

func (b *Bot) startRemoveModeTarget(ctx context.Context, chatID int64, target *models.Message) {
	b.replaceSession(chatID, &pending{Mode: "remove"})
	b.sendTarget(ctx, chatID, target, "请输入要删除的域名或 URL：", cancelMenu())
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
	b.replaceSession(chatID, &pending{Mode: "add", OriginalDomain: domainName, Domain: domainName, RootDomain: domain.RuleRoot(domainName)})
	b.send(ctx, chatID, "请选择规则动作：", keyboard([][]button{
		{{Text: "🟢 直连", Data: "route:direct"}, {Text: "🔴 代理", Data: "route:proxy"}},
		{{Text: "✖️ 取消", Data: "nav:cancel"}},
	}))
}

func (b *Bot) handleCallback(ctx context.Context, query *models.CallbackQuery) {
	message := callbackMessage(query)
	if query == nil || message == nil {
		return
	}
	unlock := b.lockChat(message.Chat.ID)
	defer unlock()
	if !b.allowed(query.From.ID, message.Chat.Type == models.ChatTypePrivate) {
		log.Printf("telegram callback rejected from=%d chat_type=%s", query.From.ID, message.Chat.Type)
		return
	}
	if b.callbackAlreadyHandled(query.ID) {
		b.answer(ctx, query.ID)
		log.Printf("telegram callback deduplicated id=%s from=%d data=%q", query.ID, query.From.ID, query.Data)
		return
	}
	b.answer(ctx, query.ID)
	chatID := message.Chat.ID
	if !b.bindSessionOwner(chatID, query.From.ID) {
		log.Printf("telegram session ownership mismatch chat=%d from=%d", chatID, query.From.ID)
		return
	}

	switch query.Data {
	case "menu:query":
		b.startQueryModeTarget(ctx, chatID, message)
		return
	case "menu:add:direct":
		b.startAddModeTarget(ctx, chatID, rules.Direct, message)
		return
	case "menu:add:proxy":
		b.startAddModeTarget(ctx, chatID, rules.Proxy, message)
		return
	case "menu:remove":
		b.startRemoveModeTarget(ctx, chatID, message)
		return
	case "menu:list":
		b.resetViewSession(chatID, query.From.ID)
		b.listTarget(ctx, chatID, message)
		return
	case "menu:status":
		b.resetViewSession(chatID, query.From.ID)
		b.statusTarget(ctx, chatID, message)
		return
	case "menu:repo":
		b.resetViewSession(chatID, query.From.ID)
		b.repoTarget(ctx, chatID, message)
		return
	case "menu:help":
		b.resetViewSession(chatID, query.From.ID)
		b.sendTarget(ctx, chatID, message, b.helpText(), homeEditMenu())
		return
	case "nav:home:edit":
		b.resetViewSession(chatID, query.From.ID)
		b.sendTarget(ctx, chatID, message, b.welcomeText(), mainMenu())
		return
	case "nav:home":
		b.clear(chatID)
		b.send(ctx, chatID, b.welcomeText(), mainMenu())
		return
	case "nav:back":
		b.back(ctx, chatID, message)
		return
	case "nav:cancel", "cancel", "confirm:no":
		b.clear(chatID)
		b.sendTarget(ctx, chatID, message, "已取消当前操作。", homeEditMenu())
		return
	}
	if strings.HasPrefix(query.Data, "dns:details:") {
		b.sendDNSDetailsTarget(ctx, chatID, query.Data, message)
		return
	}

	b.mu.Lock()
	pendingRule := b.sessions[chatID]
	b.mu.Unlock()
	if pendingRule == nil {
		b.send(ctx, chatID, "操作已过期，请返回主菜单重新开始。", errorMenu())
		return
	}
	if strings.HasPrefix(query.Data, "domain:pick:") {
		var n int
		if _, err := fmt.Sscanf(query.Data, "domain:pick:%d", &n); err != nil || n < 0 || n >= len(pendingRule.Candidates) {
			b.send(ctx, chatID, "域名候选已过期，请重新发送。", errorMenu())
			return
		}
		domainName := pendingRule.Candidates[n]
		pendingRule.Candidates = nil
		b.acceptDomainTarget(ctx, chatID, pendingRule, domainName, message)
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
		b.showAdvancedMenuTarget(ctx, chatID, pendingRule, message)
		return
	case "advanced:keyword", "advanced:wildcard", "advanced:regex":
		pendingRule.AdvancedMatch = rules.Match(strings.TrimPrefix(query.Data, "advanced:"))
		pendingRule.AdvancedSuggest = advancedSuggestion(pendingRule.AdvancedMatch, pendingRule.OriginalDomain, pendingRule.RootDomain)
		b.showAdvancedOptionTarget(ctx, chatID, pendingRule, message)
		return
	case "advanced:back":
		b.showMatchMenuTarget(ctx, chatID, pendingRule, message)
		return
	case "advanced:use":
		value, err := rules.ValidatePattern(pendingRule.AdvancedMatch, pendingRule.AdvancedSuggest)
		if err != nil {
			b.send(ctx, chatID, "建议规则生成失败："+err.Error(), cancelMenu())
			return
		}
		pendingRule.Domain = value
		pendingRule.Match = pendingRule.AdvancedMatch
		b.showAdvancedConfirmationTarget(ctx, chatID, pendingRule, message)
		return
	case "advanced:custom":
		pendingRule.AwaitingPattern = true
		b.sendTarget(ctx, chatID, message, advancedInputPrompt(pendingRule.AdvancedMatch), cancelMenu())
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
	case "remove:confirm":
		b.confirmRemoval(ctx, chatID, pendingRule)
		return
	default:
		log.Printf("telegram callback ignored: unknown data=%q", query.Data)
		b.send(ctx, chatID, "无法识别这个按钮，请返回主菜单。", errorMenu())
		return
	}

	if pendingRule.Action != "" && pendingRule.Match == "" {
		b.showMatchMenuTarget(ctx, chatID, pendingRule, message)
		return
	}
	if pendingRule.Action != "" && pendingRule.Match != "" {
		b.commitPending(ctx, chatID, query.From.ID, pendingRule, false)
	}
}

func (b *Bot) lockChat(chatID int64) func() {
	value, _ := b.chatLocks.LoadOrStore(chatID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (b *Bot) callbackAlreadyHandled(id string) bool {
	if id == "" {
		return false
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seenCallbacks == nil {
		b.seenCallbacks = make(map[string]time.Time)
	}
	for key, at := range b.seenCallbacks {
		if now.Sub(at) > 10*time.Minute {
			delete(b.seenCallbacks, key)
		}
	}
	if _, ok := b.seenCallbacks[id]; ok {
		return true
	}
	b.seenCallbacks[id] = now
	return false
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
	store, err := b.service.LoadStore(ctx)
	if err != nil {
		b.clear(chatID)
		b.send(ctx, chatID, "读取待删除规则失败："+err.Error(), homeMenu())
		return
	}
	matched := rulesForDomain(store, domainName)
	if len(matched) == 0 {
		b.clear(chatID)
		b.send(ctx, chatID, "删除失败：未找到该域名对应的个人规则。", homeMenu())
		return
	}
	b.replaceSession(chatID, &pending{Mode: "remove_confirm", Domain: domainName, OriginalDomain: domainName, DeleteRules: matched})
	b.send(ctx, chatID, removalConfirmationText(domainName, matched), keyboard([][]button{
		{{Text: "🗑️ 确认删除", Data: "remove:confirm"}, {Text: "✖️ 取消", Data: "nav:cancel"}},
	}))
}

func (b *Bot) confirmRemoval(ctx context.Context, chatID int64, p *pending) {
	if p.Mode != "remove_confirm" || p.Domain == "" || len(p.DeleteRules) == 0 {
		b.send(ctx, chatID, "删除确认已过期，请重新开始。", homeMenu())
		return
	}
	if !b.markBusy(chatID) {
		b.send(ctx, chatID, "⏳ 删除操作正在处理中，请勿重复点击。", homeMenu())
		return
	}
	store, err := b.service.LoadStore(ctx)
	if err != nil {
		b.setBusy(chatID, false)
		b.send(ctx, chatID, "确认删除前读取规则失败："+err.Error(), homeMenu())
		return
	}
	current := rulesForDomain(store, p.Domain)
	if !sameRuleSet(current, p.DeleteRules) {
		b.setBusy(chatID, false)
		if len(current) == 0 {
			b.clear(chatID)
			b.send(ctx, chatID, "规则已经不存在，无需再次删除。", homeMenu())
			return
		}
		p.DeleteRules = current
		b.send(ctx, chatID, "⚠️ 待删除规则在确认期间发生了变化，请重新核对：\n\n"+removalConfirmationText(p.Domain, current), keyboard([][]button{
			{{Text: "🗑️ 再次确认删除", Data: "remove:confirm"}, {Text: "✖️ 取消", Data: "nav:cancel"}},
		}))
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("⏳ 已确认删除 %d 条规则，正在提交到%s，请稍候……", len(current), repoProviderText(b.cfg.RuleRepoProvider)), nil)
	result, err := b.service.RemoveRule(ctx, p.Domain, nil)
	b.clear(chatID)
	if err != nil {
		b.send(ctx, chatID, "删除失败："+err.Error(), homeMenu())
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("✅ 删除完成\n域名：%s\n删除规则：%d 条\ncommit：%s", p.Domain, result.Changed, short(result.Commit)), homeMenu())
}

func rulesForDomain(store rules.Store, domainName string) []rules.Rule {
	var matched []rules.Rule
	for _, rule := range store.Rules {
		if rule.Domain == domainName {
			matched = append(matched, rule)
		}
	}
	return rules.Ordered(matched)
}

func sameRuleSet(a, b []rules.Rule) bool {
	if len(a) != len(b) {
		return false
	}
	a = rules.Ordered(append([]rules.Rule(nil), a...))
	b = rules.Ordered(append([]rules.Rule(nil), b...))
	for index := range a {
		if a[index].Domain != b[index].Domain || a[index].Match != b[index].Match || a[index].Action != b[index].Action {
			return false
		}
	}
	return true
}

func removalConfirmationText(domainName string, matched []rules.Rule) string {
	var out strings.Builder
	fmt.Fprintf(&out, "⚠️ 删除规则确认（第 2 步）\n\n域名：%s\n将删除 %d 条个人规则：", domainName, len(matched))
	for index, rule := range matched {
		fmt.Fprintf(&out, "\n%d. %s → %s", index+1, rules.Token(rule), actionText(rule.Action))
	}
	out.WriteString("\n\n删除后会生成新的 Git commit；OpenClash 下次更新远程覆写后不再应用这些规则。此操作不可在 Bot 中直接撤销。")
	return out.String()
}

func (b *Bot) query(ctx context.Context, chatID int64, domainName string) {
	result, err := b.service.Query(ctx, domainName)
	if err != nil {
		b.send(ctx, chatID, "查询失败："+err.Error(), errorMenu())
		return
	}
	personal := "未找到"
	if result.PersonalError != "" {
		personal = "暂时无法读取（不影响上游与 DNS 查询）"
	} else if len(result.Personal) > 0 {
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
	suggestionDecision := smartSuggestion(result.Personal, result.Network)
	root := result.Network.Registrable
	if root == "" {
		root = "未识别"
	}
	text := fmt.Sprintf("🔍 查询域名：%s\n👤 个人规则：%s\n📚 Aethersailor：%s\n🇨🇳 GEOSITE:CN：%s\n🧱 GEOSITE:GFW：%s\n\n%s\n可注册域名：%s\n📡 中国大陆信号：%s", domainName, personal, strings.Join(upstreamText, "\n  "), strings.Join(geositeText, "；"), strings.Join(gfwText, "；"), dnsText, root, chinaSignal)
	if geoText != "" {
		text += "\n" + geoText
	}
	if suggestionDecision.Text != "" {
		text += "\n" + suggestionDecision.Text
	}
	rows := [][]button{}
	if len(result.Personal) == 0 {
		if hasProxy {
			rows = append(rows, []button{{Text: "🟢 覆写为个人直连", Data: "override:direct"}})
		}
		if hasDirect {
			rows = append(rows, []button{{Text: "🔴 覆写为个人代理", Data: "override:proxy"}})
		}
	}
	rows = append(rows, []button{{Text: "🏠 主菜单", Data: "nav:home:edit"}})
	if len(result.Network.GeoIPs) > 0 {
		rows = append(rows, []button{{Text: "📍 查看全部 IP 归属", Data: "dns:details:all"}})
	}
	if len(result.Network.Domestic.A)+len(result.Network.Domestic.AAAA) > 0 {
		rows = append(rows, []button{{Text: "🇨🇳 查看国内 DNS 详情", Data: "dns:details:domestic"}})
	}
	if len(result.Network.Foreign.A)+len(result.Network.Foreign.AAAA) > 0 {
		rows = append(rows, []button{{Text: "🌍 查看国外 DNS 详情", Data: "dns:details:foreign"}})
	}
	if suggestionDecision.ButtonAction != "" {
		upstreamAlreadyHasAction := len(result.Personal) == 0 && ((suggestionDecision.ButtonAction == rules.Direct && hasDirect) || (suggestionDecision.ButtonAction == rules.Proxy && hasProxy))
		if !upstreamAlreadyHasAction {
			rows = append(rows, []button{{Text: suggestionDecision.ButtonText, Data: "suggest:" + string(suggestionDecision.ButtonAction)}})
		}
	} else if len(result.Personal) == 0 && suggestionDecision.Kind == "both" {
		rows = append(rows, []button{
			{Text: "🟢 添加直连（需确认）", Data: "suggest:direct"},
			{Text: "🔴 添加代理（需确认）", Data: "suggest:proxy"},
		})
	}
	b.setQuerySession(chatID, domainName, &result)
	b.send(ctx, chatID, text, keyboard(rows))
}

func (b *Bot) setQuerySession(chatID int64, domainName string, result *app.QueryResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	previous := b.sessions[chatID]
	p := &pending{Mode: "query_override", OriginalDomain: domainName, Domain: domainName, RootDomain: domain.RuleRoot(domainName), QueryResult: result}
	if previous != nil {
		p.CurrentPage = previous.CurrentPage
		p.PageStack = append([]pageSnapshot(nil), previous.PageStack...)
		p.ActiveMessageID = previous.ActiveMessageID
		p.UserID = previous.UserID
	}
	b.sessions[chatID] = p
}

func (b *Bot) list(ctx context.Context, chatID int64) {
	b.listTarget(ctx, chatID, nil)
}

func (b *Bot) listTarget(ctx context.Context, chatID int64, target *models.Message) {
	store, err := b.service.LoadStore(ctx)
	if err != nil {
		b.sendTarget(ctx, chatID, target, "个人规则暂时无法读取，但不影响其它查询。\n原因："+err.Error(), homeEditMenu())
		return
	}
	if len(store.Rules) == 0 {
		b.sendTarget(ctx, chatID, target, "当前没有个人规则。", homeEditMenu())
		return
	}
	var out strings.Builder
	fmt.Fprintf(&out, "个人规则共 %d 条：\n", len(store.Rules))
	ordered := rules.Ordered(store.Rules)
	const maxListRules = 20
	for i, r := range ordered {
		if i >= maxListRules {
			fmt.Fprintf(&out, "\n… 其余 %d 条未显示", len(ordered)-i)
			break
		}
		fmt.Fprintf(&out, "%d. %s → %s\n", i+1, rules.Token(r), actionText(r.Action))
	}
	b.sendTarget(ctx, chatID, target, out.String(), homeEditMenu())
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
	b.statusTarget(ctx, chatID, nil)
}

func (b *Bot) statusTarget(ctx context.Context, chatID int64, target *models.Message) {
	store, err := b.service.LoadStore(ctx)
	storeNote := ""
	if err != nil {
		store = rules.Empty()
		storeNote = "（暂时无法读取）"
	}
	checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	idx := b.service.CheckIndexStatus(checkCtx)
	cancel()
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
	upstream := "📦 上游规则\n仓库：" + b.cfg.UpstreamRepo + "\n分支：" + b.cfg.UpstreamBranch
	ruleVersion := idx.RuleVersion
	if ruleVersion == "" {
		ruleVersion = "未生成"
	}
	indexedSHA := idx.IndexedUpstreamSHA
	if indexedSHA == "" {
		indexedSHA = "未记录"
	}
	upstream += "\n当前索引版本：" + short(ruleVersion) + "\n索引对应提交：" + short(indexedSHA)
	if !idx.UpstreamCommittedAt.IsZero() {
		upstream += "\n上游提交时间：" + idx.UpstreamCommittedAt.In(b.cfg.Location).Format("2006-01-02 15:04:05")
	} else {
		upstream += "\n上游提交时间：未知"
	}
	upstream += "\n本地同步时间：" + updated
	if !idx.UpstreamCheckedAt.IsZero() {
		upstream += "\n远端检查时间：" + idx.UpstreamCheckedAt.In(b.cfg.Location).Format("2006-01-02 15:04:05")
	}
	switch idx.UpstreamState {
	case "latest":
		upstream += "\n状态：✅ 已是最新版"
	case "stale":
		upstream += "\n状态：⚠️ 有新版本，建议点击同步"
		if idx.UpstreamSHA != "" {
			upstream += "\n远端最新提交：" + short(idx.UpstreamSHA)
		}
	case "error":
		upstream += "\n状态：⚠️ 无法确认"
		if idx.UpstreamCheckError != "" {
			upstream += "\n检查异常：" + idx.UpstreamCheckError
		}
	default:
		upstream += "\n状态：未检查"
	}
	b.sendTarget(ctx, chatID, target, fmt.Sprintf("ClashRulePilot 运行状态\n发布仓库：%s (%s)\n个人规则：%d 条%s\n磁盘查询数据库：%t / 已加载=%t\n落地规则文件：%d 个（只保留远端当前版本）\n索引规则：直连 %d · 代理 %d · 分类 %d · GEOSITE:CN %d · GEOSITE:GFW %d\n索引更新时间：%s\n索引异常：%s\n公网 DNS：启用=%t · 国内=%t · 国外=%t · 缓存=%d · 最近=%s\nDNS 异常：%s\n上游公开镜像：%t\n\n%s", b.cfg.RuleRepoProject, b.cfg.RuleRepoProvider, len(store.Rules), storeNote, idx.Enabled, idx.Loaded, idx.Sources, idx.Direct, idx.Proxy, idx.Category, idx.GeoSite, idx.GFW, updated, lastError, dns.Enabled, dns.Domestic, dns.Foreign, dns.CacheEntries, provider, dohError, b.service.SyncEnabled(), upstream), homeEditMenu())
}

func (b *Bot) helpText() string {
	return "ClashRulePilot 使用帮助\n\n支持纯域名、URL、域名:端口和完整 OpenClash/Mihomo 日志。Fake-IP 模式下会使用公网 DoH 获取真实 IP。\n\n域名匹配：\n• DOMAIN：仅完整域名，最安全，但不含子域名。\n• DOMAIN-SUFFIX：域名及所有下级，最适合网站/CDN，但主域名范围可能过大。\n• DOMAIN-KEYWORD：包含关键词即命中，灵活但容易误伤。\n• DOMAIN-WILDCARD：支持 * 和 ?，可控但规则更难理解。\n• DOMAIN-REGEX：能力最强，但最难维护且匹配成本最高。\n• GEOSITE：维护好的域名分类，依赖数据库更新。\n• RULE-SET：批量远程规则，存在更新延迟。\n\n命令兼容：/query、/add、/remove、/list、/sync、/status、/help"
}

func (b *Bot) welcomeText() string {
	return "ClashRulePilot\n\n请选择要执行的操作："
}

func (b *Bot) repo(ctx context.Context, chatID int64) {
	b.repoTarget(ctx, chatID, nil)
}

func (b *Bot) repoTarget(ctx context.Context, chatID int64, target *models.Message) {
	b.sendTarget(ctx, chatID, target, fmt.Sprintf("规则仓库\n%s\n\n个人覆写：\n%s", b.service.RepoWebURL(), b.service.RepoRawURL("openclash/personal-overwrite.ini")), homeEditMenu())
}

func (b *Bot) clear(id int64) { b.mu.Lock(); delete(b.sessions, id); b.mu.Unlock() }
func (b *Bot) bindSessionOwner(id, userID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.sessions[id]
	if p == nil {
		b.sessions[id] = &pending{Mode: "view", UserID: userID}
		return true
	}
	if p.UserID != 0 && p.UserID != userID {
		return false
	}
	p.UserID = userID
	return true
}
func (b *Bot) replaceSession(id int64, next *pending) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if current := b.sessions[id]; current != nil {
		next.ActiveMessageID = current.ActiveMessageID
		next.CurrentPage = current.CurrentPage
		next.PageStack = append([]pageSnapshot(nil), current.PageStack...)
		if next.UserID == 0 {
			next.UserID = current.UserID
		}
	}
	b.sessions[id] = next
}

func (b *Bot) resetViewSession(id, userID int64) {
	b.replaceSession(id, &pending{Mode: "view", UserID: userID})
}

func (b *Bot) ensureSession(id int64) *pending {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p := b.sessions[id]; p != nil {
		return p
	}
	p := &pending{Mode: "view"}
	b.sessions[id] = p
	return p
}

func (b *Bot) recordPage(chatID int64, messageID int, text string, kb models.ReplyMarkup, push bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.sessions[chatID]
	if p == nil {
		return
	}
	if push && p.CurrentPage != nil {
		p.PageStack = append(p.PageStack, *p.CurrentPage)
		if len(p.PageStack) > 5 {
			p.PageStack = append([]pageSnapshot(nil), p.PageStack[len(p.PageStack)-5:]...)
		}
	}
	p.CurrentPage = &pageSnapshot{Text: text, Markup: kb}
	p.ActiveMessageID = messageID
}

func (b *Bot) back(ctx context.Context, chatID int64, target *models.Message) {
	b.mu.Lock()
	p := b.sessions[chatID]
	if p == nil || len(p.PageStack) == 0 {
		b.mu.Unlock()
		b.sendTarget(ctx, chatID, target, b.welcomeText(), mainMenu())
		return
	}
	previous := p.PageStack[len(p.PageStack)-1]
	p.PageStack = p.PageStack[:len(p.PageStack)-1]
	b.mu.Unlock()
	b.sendTargetNoPush(ctx, chatID, target, previous.Text, previous.Markup)
}
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
		{{Text: "🔍 查询域名", Data: "menu:query"}, {Text: "🟢 添加直连", Data: "menu:add:direct"}},
		{{Text: "🔴 添加代理", Data: "menu:add:proxy"}, {Text: "🗑️ 删除规则", Data: "menu:remove"}},
		{{Text: "📋 规则列表", Data: "menu:list"}, {Text: "📊 运行状态", Data: "menu:status"}},
		{{Text: "🔗 规则仓库", Data: "menu:repo"}, {Text: "ℹ️ 使用帮助", Data: "menu:help"}},
	})
}
func homeMenu() *models.InlineKeyboardMarkup {
	return keyboard([][]button{{{Text: "🏠 主菜单", Data: "nav:home"}}})
}
func homeEditMenu() *models.InlineKeyboardMarkup {
	return keyboard([][]button{{{Text: "🏠 主菜单", Data: "nav:home:edit"}}})
}
func backMenu() *models.InlineKeyboardMarkup {
	return keyboard([][]button{{{Text: "↩️ 返回", Data: "nav:back"}, {Text: "🏠 主菜单", Data: "nav:home:edit"}}})
}
func errorMenu() *models.InlineKeyboardMarkup {
	return backMenu()
}
func cancelMenu() *models.InlineKeyboardMarkup {
	return keyboard([][]button{{{Text: "✖️ 取消", Data: "nav:cancel"}, {Text: "🏠 主菜单", Data: "nav:home:edit"}}})
}
func (b *Bot) send(ctx context.Context, chatID int64, text string, kb models.ReplyMarkup) {
	b.sendTarget(ctx, chatID, nil, text, kb)
}

func (b *Bot) sendTarget(ctx context.Context, chatID int64, target *models.Message, text string, kb models.ReplyMarkup) {
	b.sendTargetMode(ctx, chatID, target, text, kb, true)
}

func (b *Bot) sendTargetNoPush(ctx context.Context, chatID int64, target *models.Message, text string, kb models.ReplyMarkup) {
	b.sendTargetMode(ctx, chatID, target, text, kb, false)
}

func (b *Bot) sendTargetMode(ctx context.Context, chatID int64, target *models.Message, text string, kb models.ReplyMarkup, push bool) {
	if target != nil {
		if _, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{ChatID: chatID, MessageID: target.ID, Text: text, ReplyMarkup: kb}); err == nil {
			log.Printf("telegram message edited chat=%d message=%d", chatID, target.ID)
			b.recordPage(chatID, target.ID, text, kb, push)
			return
		} else {
			log.Printf("telegram edit failed chat=%d message=%d: %v; falling back to send", chatID, target.ID, err)
		}
	}
	msg, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text, ReplyMarkup: kb})
	if err != nil {
		log.Printf("telegram send: %v", err)
		return
	}
	log.Printf("telegram message sent chat=%d", chatID)
	if msg != nil {
		b.recordPage(chatID, msg.ID, text, kb, push)
	}
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
		return "🟢 直连"
	}
	return "🔴 代理"
}
func matchText(m rules.Match) string {
	switch m {
	case rules.Exact:
		return "🎯 仅当前域名"
	case rules.Suffix:
		return "🌿 域名及下级"
	case rules.Keyword:
		return "🔑 关键词"
	case rules.Wildcard:
		return "✳️ 通配符"
	case rules.Regex:
		return "🧩 正则表达式"
	default:
		return "未知"
	}
}
func indexActionText(a ruleindex.Action) string {
	switch a {
	case ruleindex.Direct:
		return "🟢 直连"
	case ruleindex.Proxy:
		return "🔴 代理"
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
		return "✳️ DOMAIN-WILDCARD", "可以使用 * 和 ? 描述固定域名格式", "*.example.com 通常只覆盖子域名，不包含 example.com"
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

func (b *Bot) sendDNSDetails(ctx context.Context, chatID int64, data string) {
	b.sendDNSDetailsTarget(ctx, chatID, data, nil)
}

func (b *Bot) sendDNSDetailsTarget(ctx context.Context, chatID int64, data string, target *models.Message) {
	b.mu.Lock()
	p := b.sessions[chatID]
	var domainName string
	var cached *app.QueryResult
	if p != nil {
		domainName = p.Domain
		cached = p.QueryResult
	}
	b.mu.Unlock()
	if domainName == "" {
		b.sendTarget(ctx, chatID, target, "查询结果已过期，请重新查询域名。", errorMenu())
		return
	}
	var result app.QueryResult
	if cached != nil && cached.Domain == domainName {
		result = *cached
	} else {
		queried, err := b.service.Query(ctx, domainName)
		if err != nil {
			b.sendTarget(ctx, chatID, target, "读取 DNS 详情失败："+err.Error(), errorMenu())
			return
		}
		result = queried
	}
	geo := make(map[string]lookup.GeoIPInfo, len(result.Network.GeoIPs))
	for _, info := range result.Network.GeoIPs {
		geo[info.IP] = info
	}
	var out strings.Builder
	fmt.Fprintf(&out, "📍 DNS/IP 详细结果\n查询域名：%s", domainName)
	shown := 0
	if data == "dns:details:domestic" || data == "dns:details:all" {
		out.WriteString("\n\n🇨🇳 国内 DNS\n")
		appendDNSGroupDetails(&out, result.Network.Domestic, geo, &shown)
	}
	if data == "dns:details:foreign" || data == "dns:details:all" {
		out.WriteString("\n\n🌍 国外 DNS\n")
		appendDNSGroupDetails(&out, result.Network.Foreign, geo, &shown)
	}
	b.sendTarget(ctx, chatID, target, out.String(), backMenu())
}

func appendDNSGroupDetails(out *strings.Builder, group lookup.DNSGroupResult, geo map[string]lookup.GeoIPInfo, shown *int) {
	provider := displayDNSProvider(group.Provider)
	if provider == "" {
		provider = "未使用"
	}
	status := "主端点"
	if group.UsedBackup {
		status = "备用端点"
	}
	fmt.Fprintf(out, "端点：%s\n状态：%s\n\n解析记录：", provider, status)
	if group.Error != "" && len(group.A)+len(group.AAAA) == 0 {
		fmt.Fprintf(out, "\n错误：%s", group.Error)
		return
	}
	index := 0
	total := len(group.A) + len(group.AAAA)
	for _, record := range []struct {
		kind string
		ips  []string
	}{{"A", group.A}, {"AAAA", group.AAAA}} {
		for _, ip := range record.ips {
			if *shown >= 8 {
				continue
			}
			index++
			*shown++
			info, ok := geo[ip]
			fmt.Fprintf(out, "\n%d. %s\nIP：%s", index, record.kind, ip)
			if !ok || info.Error != "" {
				if ok && info.Error != "" {
					fmt.Fprintf(out, "\n归属：检测失败（%s）", info.Error)
				} else {
					out.WriteString("\n归属：未查询")
				}
				continue
			}
			fmt.Fprintf(out, "\n归属：%s", geoPlace(info))
		}
	}
	if index == 0 {
		if total == 0 {
			out.WriteString("\n无有效 A/AAAA 地址")
		} else {
			fmt.Fprintf(out, "\n其余 %d 个地址未显示", total)
		}
	} else if total > index {
		fmt.Fprintf(out, "\n… 本组其余 %d 个地址未显示", total-index)
	}
}

func formatDNSReport(report lookup.Report) string {
	var out strings.Builder
	out.WriteString("🌐 DNS 解析摘要")
	if len(report.FakeIP) > 0 {
		fmt.Fprintf(&out, "\n本地 DNS：Fake-IP %d", len(report.FakeIP))
	} else if report.DNSError != "" {
		out.WriteString("\n本地 DNS：解析失败")
	} else {
		fmt.Fprintf(&out, "\n本地 DNS：A %d · AAAA %d", len(report.LocalA), len(report.LocalAAAA))
	}
	out.WriteString("\n🇨🇳 国内 DNS\n")
	out.WriteString(formatDNSGroupSummary(report.Domestic, "国内 DNS"))
	out.WriteString("\n🌍 国外 DNS\n")
	out.WriteString(formatDNSGroupSummary(report.Foreign, "国外 DNS"))
	fmt.Fprintf(&out, "\n合计真实地址：A %d · AAAA %d", len(report.A), len(report.AAAA))
	return out.String()
}

func formatGeoIPReport(report lookup.Report) string {
	if len(report.GeoIPs) == 0 {
		return "📍 IP 归属：未取得真实地址或 GeoIP 结果"
	}
	china, foreign, failed := 0, 0, 0
	for _, info := range report.GeoIPs {
		if info.Error != "" {
			failed++
			continue
		}
		if info.China {
			china++
		} else {
			foreign++
		}
	}
	text := fmt.Sprintf("📍 IP 归属：已检查 %d 个 · 🇨🇳 中国大陆 %d · 🌐 境外 %d", china+foreign, china, foreign)
	if failed > 0 {
		text += fmt.Sprintf(" · ⚠️ 失败 %d 个", failed)
	}
	return text
}

func formatDNSGroupSummary(group lookup.DNSGroupResult, label string) string {
	if group.Error != "" && len(group.A)+len(group.AAAA) == 0 {
		return "状态：失败（" + group.Error + "）"
	}
	provider := displayDNSProvider(group.Provider)
	status := "主端点"
	if group.UsedBackup {
		status = "备用端点"
	}
	if group.CacheHit {
		status += " · 缓存"
	}
	if provider == "" {
		provider = label
	}
	addresses := append(append([]string{}, group.A...), group.AAAA...)
	if len(addresses) > 4 {
		addresses = addresses[:4]
	}
	text := fmt.Sprintf("端点：%s\n状态：成功 · %s\nA %d · AAAA %d", provider, status, len(group.A), len(group.AAAA))
	if len(addresses) > 0 {
		text += "\nIP：" + strings.Join(addresses, "、")
		if total := len(group.A) + len(group.AAAA); total > len(addresses) {
			text += fmt.Sprintf("（另有 %d 个）", total-len(addresses))
		}
	}
	if group.Error != "" {
		text += "\n备用说明：" + group.Error
	}
	return text
}

func displayDNSProvider(provider string) string {
	switch strings.ToLower(provider) {
	case "dns.alidns.com":
		return "阿里云"
	case "doh.pub":
		return "腾讯 DNSPod"
	case "cloudflare-dns.com":
		return "Cloudflare"
	case "dns.google":
		return "Google"
	default:
		return provider
	}
}

func geoPlace(info lookup.GeoIPInfo) string {
	parts := make([]string, 0, 5)
	country := strings.TrimSpace(info.Country)
	if country == "" {
		country = strings.TrimSpace(info.CountryCode)
	}
	if country != "" {
		parts = append(parts, countryLabel(info.CountryCode, country))
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

func countryFlag(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return "🌐"
	}
	if country := countries.ByName(code); country.IsValid() {
		if flag := country.Emoji(); flag != "" {
			return flag
		}
	}
	return string([]rune{rune(0x1F1E6) + rune(code[0]-'A'), rune(0x1F1E6) + rune(code[1]-'A')})
}

func countryLabel(code, fallback string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	flag := countryFlag(code)
	if len(code) == 2 {
		if name := display.Regions(language.SimplifiedChinese).Name(language.MustParseRegion(code)); name != "" {
			if fallback != "" && fallback != name {
				return fmt.Sprintf("%s %s（%s）", flag, fallback, name)
			}
			return flag + " " + name
		}
	}
	if fallback == "" {
		fallback = "未知国家"
	}
	return flag + " " + fallback
}

type suggestionDecision struct {
	Text         string
	Kind         string
	ButtonAction rules.Action
	ButtonText   string
}

func geoSuggestion(report lookup.Report) (string, string) {
	decision := smartSuggestion(nil, report)
	return decision.Text, decision.Kind
}

func smartSuggestion(personal []rules.Rule, report lookup.Report) suggestionDecision {
	baseText, geoKind := geoSuggestionBase(report)
	decision := suggestionDecision{Text: baseText, Kind: geoKind}
	if len(personal) == 0 {
		if geoKind == "direct" {
			decision.ButtonAction = rules.Direct
			decision.ButtonText = "🟢 建议添加直连"
		} else if geoKind == "proxy" {
			decision.ButtonAction = rules.Proxy
			decision.ButtonText = "🔴 建议添加代理"
		}
		return decision
	}

	effective, mixed := effectivePersonalRule(personal)
	if effective.Action != rules.Direct && effective.Action != rules.Proxy {
		return decision
	}
	current := actionText(effective.Action)
	opposite := rules.Direct
	if effective.Action == rules.Direct {
		opposite = rules.Proxy
	}
	oppositeText := actionText(opposite)
	matchNote := ""
	if mixed {
		matchNote = fmt.Sprintf("个人规则存在父子域名重叠，当前按最具体的 %s 规则生效。", current)
	}

	switch geoKind {
	case "direct":
		if effective.Action == rules.Direct {
			decision.Text = fmt.Sprintf("💡 建议：真实 IP 均在中国大陆，当前个人规则已经是%s，无需重复添加，建议保持现有规则。%s", current, matchNote)
		} else {
			decision.Text = fmt.Sprintf("💡 建议：真实 IP 均在中国大陆，当前个人规则为%s，与当前地域判断相反。若访问确实需要直连，可切换为%s；CDN 位置可能变化，请结合实际访问确认。%s", current, actionText(rules.Direct), matchNote)
			decision.ButtonAction = rules.Direct
			decision.ButtonText = "🟢 切换为直连（建议）"
		}
	case "proxy":
		if effective.Action == rules.Proxy {
			decision.Text = fmt.Sprintf("💡 建议：真实 IP 均在中国大陆以外，通常适合%s；当前个人规则已经是%s，无需重复添加，建议保持现有规则。若确实需要调整，可切换为%s（不建议）。%s", actionText(rules.Proxy), current, oppositeText, matchNote)
			decision.ButtonAction = rules.Direct
			decision.ButtonText = "🟢 切换为直连（不建议）"
		} else {
			decision.Text = fmt.Sprintf("💡 建议：真实 IP 均在中国大陆以外，通常适合%s；当前个人规则为%s。若访问确实需要代理，可切换为%s。CDN 位置可能变化，请结合实际访问确认。%s", actionText(rules.Proxy), current, actionText(rules.Proxy), matchNote)
			decision.ButtonAction = rules.Proxy
			decision.ButtonText = "🔴 切换为代理（建议）"
		}
	case "both":
		decision.Text = fmt.Sprintf("💡 建议：当前域名同时解析到中国大陆和境外地址，可能存在 CDN 分流；当前个人规则为%s，建议先保持现有规则，不根据单次解析自动改分组。必要时可切换为%s。%s", current, oppositeText, matchNote)
		decision.ButtonAction = opposite
		decision.ButtonText = switchButtonText(opposite, "（可选）")
	default:
		decision.Text = fmt.Sprintf("💡 建议：暂时无法根据真实 IP 判断分组；当前个人规则为%s，建议保持现有规则。%s", current, matchNote)
	}
	return decision
}

func switchButtonText(action rules.Action, suffix string) string {
	if action == rules.Direct {
		return "🟢 切换为直连" + suffix
	}
	return "🔴 切换为代理" + suffix
}

func effectivePersonalRule(personal []rules.Rule) (rules.Rule, bool) {
	ordered := rules.Ordered(personal)
	if len(ordered) == 0 {
		return rules.Rule{}, false
	}
	mixed := false
	for _, rule := range ordered[1:] {
		if rule.Action != ordered[0].Action {
			mixed = true
			break
		}
	}
	return ordered[0], mixed
}

func geoSuggestionBase(report lookup.Report) (string, string) {
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
		return "💡 建议：真实 IP 均在中国大陆以外，通常建议添加到「🔴 代理」规则。CDN 位置可能变化，请结合实际访问情况确认。", "proxy"
	case china > 0 && foreign == 0:
		return "💡 建议：中国大陆真实 IP，通常建议添加到「🟢 直连」规则。若服务仍不可达，再改为「🔴 代理」。", "direct"
	default:
		return "💡 建议：该域名同时解析到中国大陆和境外地址，地域会随 CDN 变化，请人工选择「🟢 直连」或「🔴 代理」。", "both"
	}
}
