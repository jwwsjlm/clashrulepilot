package bot

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

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
	meUsername    string
}

type pageSnapshot struct {
	Text     string
	Markup   models.ReplyMarkup
	Entities []models.MessageEntity
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
	QueueID         string
}

type button struct {
	Text string
	Data string
}

var visibleDomainPattern = regexp.MustCompile(`(?i)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?`)
var visibleRuleTokenPattern = regexp.MustCompile(`(?i)\b(?:DOMAIN|DOMAIN-SUFFIX|DOMAIN-KEYWORD|DOMAIN-WILDCARD|DOMAIN-REGEX),[^\s，；]+`)
var gitHashPattern = regexp.MustCompile(`(?i)^[0-9a-f]{7,64}$`)

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
	service.SetNotifier(b.Notify)
	return b, nil
}

func (b *Bot) Run(ctx context.Context) {
	me, err := getMeWithRetry(ctx, 5, time.Second, b.api.GetMe)
	if err != nil {
		log.Printf("telegram startup check failed: %v", err)
		return
	}
	b.mu.Lock()
	b.meUsername = me.Username
	b.mu.Unlock()
	log.Printf("telegram connected bot=@%s id=%d allowlist=%v", me.Username, me.ID, b.cfg.Allowlist)
	b.api.Start(ctx)
}

func getMeWithRetry(ctx context.Context, attempts int, delay time.Duration, getMe func(context.Context) (*models.User, error)) (*models.User, error) {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		me, err := getMe(ctx)
		if err == nil {
			return me, nil
		}
		lastErr = err
		if attempt == attempts {
			break
		}
		log.Printf("telegram startup check failed attempt=%d/%d: %v", attempt, attempts, err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
		if delay < 8*time.Second {
			delay *= 2
		}
	}
	return nil, lastErr
}

func (b *Bot) Notify(chatID int64, message string) {
	go b.send(context.Background(), chatID, message, homeMenu())
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
	log.Printf("telegram message accepted update_id=%d from=%d chat=%d text_len=%d", item.ID, item.Message.From.ID, item.Message.Chat.ID, len(text))
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
			rows = append(rows, []button{{Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}})
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
		rows = append(rows, []button{{Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}})
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
		target = queryMessageTarget(p, target)
		b.queryTarget(ctx, chatID, domainName, target)
	case "remove":
		b.removeDomain(ctx, chatID, domainName)
	}
}

func queryMessageTarget(_ *pending, target *models.Message) *models.Message {
	// A domain typed by the user must start a fresh response below that input.
	// Callback-driven flows can still reuse their explicit message target.
	return target
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
	fmt.Fprintf(&text, "🧭 请选择规则覆盖范围\n\n🔍 输入域名：%s", input)
	if root != "" {
		fmt.Fprintf(&text, "\n🛡️ 安全主域名：%s", root)
	}
	fmt.Fprintf(&text, "\n\n🎯 仅当前域名\n📋 规则：DOMAIN,%s\n✅ 优点：范围最小，不会误伤其他子域名\n⚠️ 注意：123.%s 不会命中", input, input)
	rows := [][]button{{{Text: "🎯 仅当前域名", Data: "match:exact"}}}
	if root == "" {
		text.WriteString("\n\n⚠️ 无法安全识别主域名\n已隐藏后缀范围，避免误覆盖公共或共享后缀。")
	} else if root == input {
		fmt.Fprintf(&text, "\n\n🌐 整个主域名 · ⭐ 推荐\n📋 规则：DOMAIN-SUFFIX,%s\n✅ 优点：匹配主域名和所有子域名\n⚠️ 注意：www、api、cdn 等都会使用同一动作", input)
		rows = append(rows, []button{{Text: "🌐 整个主域名 · ⭐ 推荐", Data: "match:suffix"}})
	} else {
		fmt.Fprintf(&text, "\n\n🌿 当前域名及下级 · ⭐ 推荐\n📋 规则：DOMAIN-SUFFIX,%s\n✅ 优点：覆盖当前域名和所有下级域名\n⚠️ 注意：不会覆盖同主域名下的其他分支", input)
		rows = append(rows, []button{{Text: "🌿 当前域名及下级 · ⭐ 推荐", Data: "match:suffix"}})
		if root != "" {
			fmt.Fprintf(&text, "\n\n🌐 整个主域名\n📋 规则：DOMAIN-SUFFIX,%s\n✅ 优点：一次覆盖整个网站\n⚠️ 注意：所有子域名都会使用同一动作", root)
			rows = append(rows, []button{{Text: "🌐 整个主域名", Data: "match:root"}})
		}
	}
	rows = append(rows,
		[]button{{Text: "🧰 高级匹配", Data: "match:advanced"}},
		[]button{{Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}},
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
		{{Text: "↩️ 返回范围选择", Data: "advanced:back"}, {Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}},
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
		{{Text: "↩️ 返回高级匹配", Data: "match:advanced"}, {Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}},
	}))
}

func (b *Bot) showAdvancedConfirmation(ctx context.Context, chatID int64, p *pending) {
	b.showAdvancedConfirmationTarget(ctx, chatID, p, nil)
}

func (b *Bot) showAdvancedConfirmationTarget(ctx context.Context, chatID int64, p *pending, target *models.Message) {
	name, _, risk := advancedDescription(p.Match)
	text := fmt.Sprintf("⚠️ 请确认高级规则\n\n类型：%s\n准备提交：%s\n动作：%s\n\n风险：%s", name, rules.Token(toRule(p, 0)), actionText(p.Action), risk)
	b.sendTarget(ctx, chatID, target, text, keyboard([][]button{
		{{Text: "✅ 确认提交", Data: "advanced:confirm"}, {Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}},
	}))
}

func (b *Bot) commitPending(ctx context.Context, chatID, userID int64, p *pending, force bool) {
	if b.service.ReadOnly() {
		b.send(ctx, chatID, "🔒 当前仓库处于只读模式，请在运行状态中检查 Token 权限。", homeMenu())
		return
	}
	if !b.markBusy(chatID) {
		b.send(ctx, chatID, "⏳ 当前规则正在处理中，请勿重复点击。", homeMenu())
		return
	}
	verb := "正在检查冲突并提交"
	if force {
		verb = "正在移动规则并提交"
	}
	progress := b.sendProgress(ctx, chatID, fmt.Sprintf("⏳ 已选择：%s\n准备提交：%s\n%s到%s，请稍候……", matchText(p.Match), rules.Token(toRule(p, userID)), verb, repoProviderText(b.cfg.RuleRepoProvider)))
	result, err := b.service.AddRule(ctx, toRule(p, userID), force)
	if conflict, ok := err.(*app.ConflictError); ok {
		b.setBusy(chatID, false)
		b.sendTarget(ctx, chatID, progress, fmt.Sprintf("⚠️ 更改域名分组确认（第 2 步）\n\n规则：%s\n当前分组：%s\n目标分组：%s\n\n确认后会移动现有规则并生成新的 Git commit。OpenClash 下次更新远程覆写后将使用新分组。", rules.Token(conflict.Existing), actionText(conflict.Existing.Action), actionText(p.Action)), keyboard([][]button{
			{{Text: "🔄 确认更改分组", Data: "confirm:yes"}, {Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}},
		}))
		return
	}
	b.clear(chatID)
	if err != nil {
		message := err.Error()
		if strings.Contains(message, "rule already exists") {
			message = "该规则已经存在，无需重复提交"
		}
		b.sendTarget(ctx, chatID, progress, "提交失败："+message, homeMenu())
		return
	}
	if result.Queued {
		b.sendTarget(ctx, chatID, progress, fmt.Sprintf("📤 仓库暂时不可用，操作已安全加入待提交队列。\n\n规则：%s\n队列编号：%s\n\n该规则尚未在 OpenClash 生效，仓库恢复后会自动重试。", rules.Token(toRule(p, userID)), short(result.QueueID)), homeMenu())
		return
	}
	var extra string
	if len(result.Related) > 0 {
		extra = fmt.Sprintf("\n\n⚠️ 已保留 %d 条相反动作的父子/重叠规则。生成的显式规则会按精确度排序，更具体的规则优先。", len(result.Related))
	}
	publishStatus := "公共镜像：已同步"
	if result.PublicPending {
		publishStatus = "⚠️ 私有仓库已提交；公共镜像待后台重试"
	}
	b.sendTarget(ctx, chatID, progress, fmt.Sprintf("✅ 规则已提交\n\n动作：%s\n规则：%s\n覆盖：%s\ncommit：%s\n%s%s", actionText(p.Action), rules.Token(toRule(p, userID)), coverageText(p), short(result.Commit), publishStatus, extra), homeMenu())
}

func (b *Bot) startAddMode(ctx context.Context, chatID int64, action rules.Action) {
	b.startAddModeTarget(ctx, chatID, action, nil)
}

func (b *Bot) startAddModeTarget(ctx context.Context, chatID int64, action rules.Action, target *models.Message) {
	if b.service.ReadOnly() {
		b.sendTarget(ctx, chatID, target, "🔒 当前仓库处于只读模式，暂时不能添加或移动规则。", homeEditMenu())
		return
	}
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
	if b.service.ReadOnly() {
		b.send(ctx, chatID, "🔒 当前仓库处于只读模式，暂时不能添加规则。", homeMenu())
		return
	}
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
		{{Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}},
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
	case "status:selfcheck":
		b.selfCheckTarget(ctx, chatID, message)
		return
	case "queue:list":
		b.queueTarget(ctx, chatID, message, 0)
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
		b.sendTarget(ctx, chatID, message, b.welcomeText(), mainMenu())
		return
	}
	if strings.HasPrefix(query.Data, "queue:page:") {
		var page int
		if _, err := fmt.Sscanf(query.Data, "queue:page:%d", &page); err == nil {
			b.queueTarget(ctx, chatID, message, page)
			return
		}
	}
	if strings.HasPrefix(query.Data, "queue:cancel:") {
		id := strings.TrimPrefix(query.Data, "queue:cancel:")
		b.replaceSession(chatID, &pending{Mode: "queue_cancel", QueueID: id, UserID: query.From.ID})
		b.sendTarget(ctx, chatID, message, "⚠️ 确认取消待提交操作？\n\n队列编号："+short(id)+"\n取消后该操作不会提交到规则仓库。", keyboard([][]button{{{Text: "🗑️ 确认取消", Data: "queue:confirm:" + id}, {Text: "↩️ 返回", Data: "queue:list"}}}))
		return
	}
	if strings.HasPrefix(query.Data, "queue:confirm:") {
		id := strings.TrimPrefix(query.Data, "queue:confirm:")
		if err := b.service.CancelQueued(id, query.From.ID); err != nil {
			b.sendTarget(ctx, chatID, message, "取消队列操作失败："+err.Error(), errorMenu())
		} else {
			b.queueTarget(ctx, chatID, message, 0)
		}
		return
	}
	if strings.HasPrefix(query.Data, "queue:force:") {
		id := strings.TrimPrefix(query.Data, "queue:force:")
		b.sendTarget(ctx, chatID, message, "⚠️ 确认按排队时的目标分组覆盖远端最新规则？\n\n队列编号："+short(id)+"\n确认后会在仓库恢复可写时重新提交。", keyboard([][]button{{{Text: "🔄 确认重新提交", Data: "queue:force-confirm:" + id}, {Text: "↩️ 返回", Data: "queue:list"}}}))
		return
	}
	if strings.HasPrefix(query.Data, "queue:force-confirm:") {
		id := strings.TrimPrefix(query.Data, "queue:force-confirm:")
		if err := b.service.ResumeQueued(id, true, query.From.ID); err != nil {
			b.sendTarget(ctx, chatID, message, "恢复队列操作失败："+err.Error(), errorMenu())
		} else {
			b.queueTarget(ctx, chatID, message, 0)
		}
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
	if query.Data == "query:remove" {
		if pendingRule.Mode != "query_override" || pendingRule.Domain == "" {
			b.sendTarget(ctx, chatID, message, "查询会话已过期，请重新查询域名。", errorMenu())
			return
		}
		b.removeDomain(ctx, chatID, pendingRule.Domain)
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
	if b.service.ReadOnly() {
		b.send(ctx, chatID, "🔒 当前仓库处于只读模式，暂时不能删除规则。", homeMenu())
		return
	}
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
		{{Text: "🗑️ 确认删除", Data: "remove:confirm"}, {Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}},
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
			{{Text: "🗑️ 再次确认删除", Data: "remove:confirm"}, {Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}},
		}))
		return
	}
	progress := b.sendProgress(ctx, chatID, fmt.Sprintf("⏳ 已确认删除 %d 条规则，正在提交到%s，请稍候……", len(current), repoProviderText(b.cfg.RuleRepoProvider)))
	result, err := b.service.RemoveRule(ctx, p.Domain, nil, p.UserID)
	b.clear(chatID)
	if err != nil {
		b.sendTarget(ctx, chatID, progress, "删除失败："+err.Error(), homeMenu())
		return
	}
	if result.Queued {
		b.sendTarget(ctx, chatID, progress, fmt.Sprintf("📤 仓库暂时不可用，删除操作已加入待提交队列。\n队列编号：%s\n\n规则尚未从 OpenClash 生效文件中删除。", short(result.QueueID)), homeMenu())
		return
	}
	publishStatus := "公共镜像：已同步"
	if result.PublicPending {
		publishStatus = "⚠️ 私有仓库已提交；公共镜像待后台重试"
	}
	b.sendTarget(ctx, chatID, progress, fmt.Sprintf("✅ 删除完成\n域名：%s\n删除规则：%d 条\ncommit：%s\n%s", p.Domain, result.Changed, short(result.Commit), publishStatus), homeMenu())
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
	b.queryTarget(ctx, chatID, domainName, nil)
}

func (b *Bot) queryTarget(ctx context.Context, chatID int64, domainName string, target *models.Message) {
	target = b.sendTargetModeEntities(ctx, chatID, target, "⏳ 正在查询："+domainName+"\n正在检查个人规则、上游规则、DNS 与 IP 归属，请稍候……", nil, nil, false)
	var progressMu sync.Mutex
	progressState := app.QueryProgress{Stage: "start"}
	progressFinished := make(chan struct{})
	timer := time.AfterFunc(b.cfg.QueryProgressInterval, func() {
		defer close(progressFinished)
		progressMu.Lock()
		state := progressState
		progressMu.Unlock()
		b.sendTargetModeEntities(ctx, chatID, target, queryProgressText(domainName, state), nil, nil, false)
	})
	result, err := b.service.QueryWithProgress(ctx, domainName, func(update app.QueryProgress) {
		progressMu.Lock()
		progressState = update
		progressMu.Unlock()
	})
	if !timer.Stop() {
		<-progressFinished
	}
	if err != nil {
		b.sendTargetModeEntities(ctx, chatID, target, "查询失败："+err.Error(), errorMenu(), nil, target == nil)
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
	entities := whoisEntities(text, domainName, root)
	entities = append(entities, exactTextLinkEntities(text, "Aethersailor", upstreamRepositoryURL(b.cfg.UpstreamRepo))...)
	if geoText != "" {
		text += "\n" + geoText
	}
	if suggestionDecision.Text != "" {
		text += "\n" + suggestionDecision.Text
	}
	text += fmt.Sprintf("\n\n⏱ 查询耗时：%s\n👤 个人 %s · 📚 上游 %s · 🏠 本地 DNS %s\n🇨🇳 国内 DNS %s · 🌍 国外 DNS %s · 📍 GeoIP %s",
		formatDuration(result.Timing.Total), formatDuration(result.Timing.Personal), formatDuration(result.Timing.Upstream), formatDuration(result.Timing.LocalDNS),
		formatDuration(result.Timing.DomesticDNS), formatDuration(result.Timing.ForeignDNS), formatDuration(result.Timing.GeoIP))
	if result.Timing.Shared {
		text += " · 🔗 已复用相同域名查询"
	}
	rows := [][]button{}
	canWrite := !b.service.ReadOnly()
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
	if canWrite && len(result.Personal) == 0 {
		rows = append(rows, queryActionButtons(suggestionDecision, hasDirect, hasProxy))
	} else if canWrite && suggestionDecision.ButtonAction != "" {
		rows = append(rows, []button{{Text: suggestionDecision.ButtonText, Data: "suggest:" + string(suggestionDecision.ButtonAction)}})
	}
	if canWrite && len(result.Personal) > 0 {
		rows = append(rows, []button{{Text: "🗑️ 删除匹配的个人规则", Data: "query:remove"}})
	}
	b.setQuerySession(chatID, domainName, &result)
	b.sendTargetModeEntities(ctx, chatID, target, text, keyboard(rows), entities, target == nil)
}

func queryProgressText(domainName string, progress app.QueryProgress) string {
	t := progress.Timing
	personal := "🔄 查询中"
	upstream := "⏸ 等待"
	localDNS := "⏸ 等待"
	domesticDNS := "⏸ 等待"
	foreignDNS := "⏸ 等待"
	geoIP := "⏸ 等待 DNS"
	if progress.Stage != "start" {
		personal = "✅ 完成 · " + formatDuration(t.Personal)
	}
	if progress.Stage == "upstream" || progress.Stage == "network" || progress.Stage == "complete" {
		upstream = "✅ 完成 · " + formatDuration(t.Upstream)
	}
	if progress.Stage == "network" {
		localDNS = "🔄 查询中"
		domesticDNS = "🔄 查询中"
		foreignDNS = "🔄 查询中"
		geoIP = "⏸ 等待 DNS"
	}
	if progress.Stage == "complete" {
		localDNS = "✅ 完成 · " + formatDuration(t.LocalDNS)
		domesticDNS = "✅ 完成 · " + formatDuration(t.DomesticDNS)
		foreignDNS = "✅ 完成 · " + formatDuration(t.ForeignDNS)
		geoIP = "✅ 完成 · " + formatDuration(t.GeoIP)
	}
	return fmt.Sprintf("⏳ 正在查询：%s\n\n👤 个人规则：%s\n📚 Aethersailor/GEOSITE：%s\n🏠 本地 DNS：%s\n🇨🇳 国内 DNS：%s\n🌍 国外 DNS：%s\n📍 GeoIP：%s\n\n已用时：%s", domainName, personal, upstream, localDNS, domesticDNS, foreignDNS, geoIP, formatDuration(t.Total))
}

func formatDuration(value time.Duration) string {
	if value < time.Millisecond {
		return "<1ms"
	}
	if value < time.Second {
		return fmt.Sprintf("%dms", value.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", value.Seconds())
}

func whoisEntities(text, domainName, registrable string) []models.MessageEntity {
	targets := []struct {
		label  string
		domain string
	}{
		{label: "🔍 查询域名：", domain: domainName},
		{label: "可注册域名：", domain: registrable},
	}
	entities := make([]models.MessageEntity, 0, len(targets))
	for _, target := range targets {
		if target.domain == "" || target.domain == "未识别" {
			continue
		}
		needle := target.label + target.domain
		start := strings.Index(text, needle)
		if start < 0 {
			continue
		}
		start += len(target.label)
		entities = append(entities, models.MessageEntity{
			Type:   models.MessageEntityTypeTextLink,
			Offset: telegramTextLength(text[:start]),
			Length: telegramTextLength(target.domain),
			URL:    "https://who.is/whois/" + url.PathEscape(target.domain),
		})
	}
	return entities
}

func exactTextLinkEntities(text, label, targetURL string) []models.MessageEntity {
	if label == "" || targetURL == "" {
		return nil
	}
	start := strings.Index(text, label)
	if start < 0 {
		return nil
	}
	return []models.MessageEntity{{
		Type:   models.MessageEntityTypeTextLink,
		Offset: telegramTextLength(text[:start]),
		Length: telegramTextLength(label),
		URL:    targetURL,
	}}
}

func upstreamRepositoryURL(repo string) string {
	repo = strings.Trim(repo, "/")
	if repo == "" {
		return ""
	}
	return (&url.URL{Scheme: "https", Host: "github.com", Path: "/" + repo}).String()
}

func upstreamCommitURL(repo, sha string) string {
	sha = strings.TrimSpace(sha)
	if !gitHashPattern.MatchString(sha) {
		return ""
	}
	base := upstreamRepositoryURL(repo)
	if base == "" {
		return ""
	}
	return base + "/commit/" + sha
}

func telegramTextLength(value string) int {
	return len(utf16.Encode([]rune(value)))
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
	progress := b.sendProgress(ctx, chatID, "🔄 正在同步 Aethersailor、GEOSITE:CN 与 GEOSITE:GFW 本地索引，请稍候……")
	result, err := b.service.SyncWithSource(ctx, "telegram")
	if err != nil {
		b.sendTarget(ctx, chatID, progress, "同步失败："+err.Error(), homeMenu())
		return
	}
	msg := "同步完成：本地索引无变化"
	if result.IndexChanged {
		msg = "同步完成：本地索引已更新"
	}
	if result.Commit != "" {
		msg += fmt.Sprintf("\n镜像 %d 个文件，commit=%s", result.Changed, short(result.Commit))
	}
	if result.PublicPending {
		msg += "\n⚠️ 私有仓库已更新；公共镜像待后台重试"
	}
	state := b.service.SyncStatus()
	if state.Shared {
		msg += "\n🔗 已复用正在运行的同步任务"
	}
	if state.Duration > 0 {
		msg += "\n耗时：" + formatDuration(state.Duration)
	}
	b.sendTarget(ctx, chatID, progress, msg, homeMenu())
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
	access := b.service.AccessStatus()
	publicAccess := b.service.PublicAccessStatus()
	queued, queueErr := b.service.QueueForUser(chatID)
	syncState := b.service.SyncStatus()
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
	b.mu.Lock()
	username := b.meUsername
	b.mu.Unlock()
	if username == "" {
		username = "尚未连接"
	} else {
		username = "@" + username
	}
	mode := "✅ 可读写"
	if !access.Writable {
		mode = "🔒 只读"
	}
	accessError := access.Error
	if accessError == "" {
		accessError = "无"
	}
	queueText := fmt.Sprintf("%d 条", len(queued))
	if queueErr != nil {
		queueText = "读取失败"
	}
	syncText := "空闲"
	if syncState.Running {
		syncText = "🔄 运行中 · " + syncState.Source
		if syncState.Stage != "" {
			syncText += " · " + syncState.Stage
		}
	} else if syncState.Duration > 0 {
		syncText = "最近耗时 " + formatDuration(syncState.Duration)
	}
	if syncState.LastError != "" {
		syncText += " · ⚠️ " + syncState.LastError
	}
	rawState := fmt.Sprintf("%t", publicAccess.RawAccessible)
	if publicAccess.RawError != "" {
		rawState += "（" + publicAccess.RawError + "）"
	}
	text := fmt.Sprintf("ClashRulePilot 运行状态\n🔐 私有规则源：%s (%s)\n🌐 公共发布镜像：%s (%s)\n👤 个人规则：%d 条%s\n📤 待提交队列：%s\n磁盘查询数据库：%t / 已加载=%t\n落地规则文件：%d 个（只保留远端当前版本）\n索引规则：直连 %d · 代理 %d · 分类 %d · GEOSITE:CN %d · GEOSITE:GFW %d\n索引更新时间：%s\n索引异常：%s\n公网 DNS：启用=%t · 国内=%t · 国外=%t · 缓存=%d · 最近=%s\nDNS 异常：%s\n同步任务：%s\n容器上游公开镜像：%t\n\n🔐 凭据与权限\nTelegram：%s\n私有源：认证=%t · 读取=%t · 写入=%t · commit=%s\n公共镜像：读取=%t · 写入=%t · Public=%t · commit=%s\n公共 Raw：%s\n运行模式：%s\n权限异常：%s\n\n%s", b.cfg.RuleRepoProject, b.cfg.RuleRepoProvider, b.cfg.PublicRuleRepoProject, b.cfg.PublicRuleRepoProvider, len(store.Rules), storeNote, queueText, idx.Enabled, idx.Loaded, idx.Sources, idx.Direct, idx.Proxy, idx.Category, idx.GeoSite, idx.GFW, updated, lastError, dns.Enabled, dns.Domestic, dns.Foreign, dns.CacheEntries, provider, dohError, syncText, b.service.SyncEnabled(), username, access.Authenticated, access.Readable, access.Writable, short(access.Revision), publicAccess.Readable, publicAccess.Writable, publicAccess.Public, short(publicAccess.Revision), rawState, mode, accessError, upstream)
	entities := exactTextLinkEntities(text, b.cfg.RuleRepoProject, b.service.PrivateRepoWebURL())
	entities = append(entities, exactTextLinkEntities(text, b.cfg.PublicRuleRepoProject, b.service.RepoWebURL())...)
	entities = append(entities, exactTextLinkEntities(text, b.cfg.UpstreamRepo, upstreamRepositoryURL(b.cfg.UpstreamRepo))...)
	indexedCommitURL := upstreamCommitURL(b.cfg.UpstreamRepo, indexedSHA)
	entities = append(entities, exactTextLinkEntities(text, short(ruleVersion), indexedCommitURL)...)
	entities = append(entities, exactTextLinkEntities(text, short(indexedSHA), indexedCommitURL)...)
	entities = append(entities, exactTextLinkEntities(text, short(idx.UpstreamSHA), upstreamCommitURL(b.cfg.UpstreamRepo, idx.UpstreamSHA))...)
	b.sendTargetModeEntities(ctx, chatID, target, text, statusMenu(len(queued)), entities, true)
}

func statusMenu(queueCount int) *models.InlineKeyboardMarkup {
	rows := [][]button{{{Text: "🧪 OpenClash 覆写自检", Data: "status:selfcheck"}}}
	if queueCount > 0 {
		rows = append(rows, []button{{Text: fmt.Sprintf("📤 待提交队列 · %d", queueCount), Data: "queue:list"}})
	}
	rows = append(rows, []button{{Text: "🏠 主菜单", Data: "nav:home:edit"}})
	return keyboard(rows)
}

func (b *Bot) selfCheckTarget(ctx context.Context, chatID int64, target *models.Message) {
	b.sendTargetModeEntities(ctx, chatID, target, "🧪 正在检查 OpenClash 个人覆写……\n正在验证仓库、Raw 地址、INI/YAML 结构和规则一致性。", nil, nil, false)
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	report := b.service.SelfCheck(checkCtx)
	cancel()
	var out strings.Builder
	out.WriteString("🧪 OpenClash 覆写自检\n")
	for _, item := range report.Items {
		icon := "❌"
		if item.Level == "ok" {
			icon = "✅"
		} else if item.Level == "warn" {
			icon = "⚠️"
		}
		fmt.Fprintf(&out, "\n%s %s", icon, item.Text)
	}
	fmt.Fprintf(&out, "\n\n结果：通过 %d · 警告 %d · 失败 %d", report.Passed, report.Warnings, report.Failed)
	out.WriteString("\n\nLuCI 确认路径：\n服务 → OpenClash → 配置订阅 → 编辑当前订阅 → 远程覆写")
	if report.Failed > 0 {
		out.WriteString("\n\n仍未生效时：先到 系统 → 软件包 检查依赖，再到 服务 → OpenClash → 插件设置 → 调试日志 → 生成。")
	}
	b.sendTarget(ctx, chatID, target, out.String(), keyboard([][]button{{{Text: "↩️ 返回运行状态", Data: "menu:status"}, {Text: "🏠 主菜单", Data: "nav:home:edit"}}}))
}

func (b *Bot) queueTarget(ctx context.Context, chatID int64, target *models.Message, page int) {
	items, err := b.service.QueueForUser(chatID)
	if err != nil {
		b.sendTarget(ctx, chatID, target, "读取待提交队列失败："+err.Error(), errorMenu())
		return
	}
	const size = 8
	if page < 0 {
		page = 0
	}
	pages := (len(items) + size - 1) / size
	if pages == 0 {
		pages = 1
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * size
	end := min(start+size, len(items))
	var out strings.Builder
	fmt.Fprintf(&out, "📤 待提交队列\n共 %d 条 · 第 %d/%d 页", len(items), page+1, pages)
	rows := [][]button{}
	for index := start; index < end; index++ {
		item := items[index]
		targetText := item.Domain
		if item.Operation == "add" {
			targetText = rules.Token(item.Rule)
		}
		fmt.Fprintf(&out, "\n\n%d. %s\n状态：%s · 重试 %d 次", index+1, targetText, item.Status, item.Attempts)
		if item.LastError != "" {
			fmt.Fprintf(&out, "\n原因：%s", item.LastError)
		}
		row := []button{{Text: "🗑️ 取消 · " + short(item.ID), Data: "queue:cancel:" + item.ID}}
		if item.Status == "paused" {
			row = append([]button{{Text: "🔄 重新确认", Data: "queue:force:" + item.ID}}, row...)
		}
		rows = append(rows, row)
	}
	if len(items) == 0 {
		out.WriteString("\n\n当前没有等待提交的操作。")
	}
	var nav []button
	if page > 0 {
		nav = append(nav, button{Text: "⬅️ 上一页", Data: fmt.Sprintf("queue:page:%d", page-1)})
	}
	if page+1 < pages {
		nav = append(nav, button{Text: "➡️ 下一页", Data: fmt.Sprintf("queue:page:%d", page+1)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []button{{Text: "↩️ 返回运行状态", Data: "menu:status"}, {Text: "🏠 主菜单", Data: "nav:home:edit"}})
	b.sendTarget(ctx, chatID, target, out.String(), keyboard(rows))
}

func (b *Bot) helpText() string {
	return "🧭 ClashRulePilot 使用帮助\n\n" +
		"🔍 推荐流程\n" +
		"1. 点击「查询域名」，发送纯域名、URL、域名:端口，或完整 OpenClash/Mihomo 日志。\n" +
		"2. 查看个人规则、Aethersailor、国内/国外 DNS 和 IP 归属。\n" +
		"3. 根据结果选择「添加直连」或「添加代理」，再选择匹配范围并确认提交。\n" +
		"4. 规则提交成功后，点击「规则仓库」获取最新 Raw 地址。\n\n" +
		"📘 个人规则接入 OpenClash\n" +
		"① 点击本 Bot 主菜单的「🔗 规则仓库」，复制「个人覆写」Raw 地址。\n" +
		"② LuCI 路径：服务 → OpenClash → 运行状态 → 顶部「覆写模块」按钮。\n" +
		"③ 在覆写编辑器点击「+」→ Subscribe，类型选择远程/HTTP，粘贴 Raw 地址。\n" +
		"④ 目标配置选择「所有配置文件」（对应 config=all），启用模块并保存；config 留空会导致覆写永不生效。\n" +
		"⑤ 点击模块刷新后，重启 OpenClash。可在 服务 → OpenClash → 配置管理 → 当前配置 → 下载运行配置，检查 rules 顶部是否出现个人规则。\n" +
		"⑥ 若同时存在其他会修改 rules 的覆写模块，请让个人模块在其之后执行（覆写 order 数值越小越晚执行），确保个人 +rules 位于最前面。\n\n" +
		"📗 Clash/Mihomo 标准规则集\n" +
		"规则仓库同时发布 My_Direct_Classical.yaml 和 My_Proxy_Classical.yaml。普通 Clash/Mihomo 在 rule-providers 中使用 behavior=classical、format=yaml，并通过 RULE-SET 指定 DIRECT 或代理策略组。Provider 文件是标准 YAML，不包含 [YAML] 段头。\n\n" +
		"🧹 策略组清理脚本\n" +
		"如果重度分流模板让“🚀 手动选择”同时显示单节点和节点组，可下载 openclash_custom_overwrite.sh 放到路由器 /etc/openclash/custom/。脚本只保留节点组引用，不修改其他策略组；Ruby/YAML 依赖缺失时会跳过且不阻断启动。\n\n" +
		"🧠 规则原理\n" +
		"个人覆写文件使用 [YAML] 段和 +rules，将个人规则插入订阅规则之前；Mihomo 按 rules 从上到下匹配，因此更具体、排在前面的个人规则可覆盖上游规则。\n\n" +
		"🧩 匹配方式\n" +
		"• DOMAIN：仅完整域名，范围最小、误匹配最低；不包含子域名。\n" +
		"• DOMAIN-SUFFIX：当前域名及所有下级，适合网站/CDN；主域名范围可能过大。\n" +
		"• DOMAIN-KEYWORD：域名包含关键词即命中，灵活但容易误伤。\n" +
		"• DOMAIN-WILDCARD：支持 * 和 ?，比关键词可控；规则较难理解。\n" +
		"• DOMAIN-REGEX：表达能力最强；最难维护，写错也更难排查。\n" +
		"• GEOSITE：维护好的分类数据库，依赖数据库更新，不适合临时个人域名。\n" +
		"• RULE-SET：批量引用规则集合，便于共享，但存在远程依赖和更新延迟。\n\n" +
		"🛠️ 排障\n" +
		"先检查 系统 → 软件包 的依赖；仍异常时到 服务 → OpenClash → 插件设置 → 调试日志 → 生成，查看依赖、覆写、YAML、DNS 和防火墙信息。\n\n" +
		"⌨️ 命令兼容：/query、/add、/remove、/list、/sync、/status、/help"
}

func (b *Bot) welcomeText() string {
	return "🧭 ClashRulePilot\n\n" +
		"🔍 第一步：查询域名\n" +
		"发送域名、URL、域名:端口，或完整 OpenClash 日志。\n\n" +
		"🔎 查询内容\n" +
		"👤 个人规则 · 📚 上游规则\n" +
		"🌐 国内/国外 DNS · 📍 IP 归属\n\n" +
		"查询完成后，再选择 🟢 直连 或 🔴 代理。"
}

func (b *Bot) repo(ctx context.Context, chatID int64) {
	b.repoTarget(ctx, chatID, nil)
}

func (b *Bot) repoTarget(ctx context.Context, chatID int64, target *models.Message) {
	b.sendTarget(ctx, chatID, target, fmt.Sprintf("规则仓库\n%s\n\nSublink Pro/Subconverter 远程 ACL：\n%s\n\n纯文本直连 ACL：\n%s\n\n纯文本代理 ACL：\n%s\n\nOpenClash 远程覆写：\n%s\n\nClash/Mihomo 直连 Provider：\n%s\n\nClash/Mihomo 代理 Provider：\n%s\n\n策略组清理脚本：\n%s", b.service.RepoWebURL(), b.service.RepoRawURL("clash/Custom_Mihomo_Optimized.ini"), b.service.RepoRawURL("rules/acl/Custom_Direct.list"), b.service.RepoRawURL("rules/acl/Custom_Proxy.list"), b.service.RepoRawURL("openclash/personal-overwrite.ini"), b.service.RepoRawURL("rules/personal/My_Direct_Classical.yaml"), b.service.RepoRawURL("rules/personal/My_Proxy_Classical.yaml"), b.service.RepoRawURL("openclash/openclash_custom_overwrite.sh")), homeEditMenu())
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

func (b *Bot) recordPage(chatID int64, messageID int, text string, kb models.ReplyMarkup, entities []models.MessageEntity, push bool) {
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
	p.CurrentPage = &pageSnapshot{Text: text, Markup: kb, Entities: append([]models.MessageEntity(nil), entities...)}
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
	b.sendTargetModeEntities(ctx, chatID, target, previous.Text, previous.Markup, previous.Entities, false)
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
		{{Text: "🔍 查询域名", Data: "menu:query"}, {Text: "📊 运行状态", Data: "menu:status"}},
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
	return keyboard([][]button{{{Text: "✖️ 取消并返回主菜单", Data: "nav:cancel"}}})
}
func (b *Bot) send(ctx context.Context, chatID int64, text string, kb models.ReplyMarkup) {
	b.sendTarget(ctx, chatID, nil, text, kb)
}

func (b *Bot) sendProgress(ctx context.Context, chatID int64, text string) *models.Message {
	entities := addDomainCodeEntities(text, nil)
	msg, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text, Entities: entities})
	if err != nil {
		log.Printf("telegram progress send: %v", err)
		return nil
	}
	if msg == nil {
		log.Printf("telegram progress send returned no message chat=%d", chatID)
		return nil
	}
	log.Printf("telegram progress message sent chat=%d message=%d", chatID, msg.ID)
	return msg
}

func (b *Bot) sendTarget(ctx context.Context, chatID int64, target *models.Message, text string, kb models.ReplyMarkup) {
	b.sendTargetMode(ctx, chatID, target, text, kb, true)
}

func (b *Bot) sendTargetMode(ctx context.Context, chatID int64, target *models.Message, text string, kb models.ReplyMarkup, push bool) {
	b.sendTargetModeEntities(ctx, chatID, target, text, kb, nil, push)
}

func (b *Bot) sendTargetModeEntities(ctx context.Context, chatID int64, target *models.Message, text string, kb models.ReplyMarkup, entities []models.MessageEntity, push bool) *models.Message {
	entities = addDomainCodeEntities(text, entities)
	previewDisabled := true
	preview := &models.LinkPreviewOptions{IsDisabled: &previewDisabled}
	if target != nil {
		if _, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{ChatID: chatID, MessageID: target.ID, Text: text, Entities: entities, LinkPreviewOptions: preview, ReplyMarkup: kb}); err == nil {
			log.Printf("telegram message edited chat=%d message=%d", chatID, target.ID)
			b.recordPage(chatID, target.ID, text, kb, entities, push)
			return target
		} else {
			log.Printf("telegram edit failed chat=%d message=%d: %v; falling back to send", chatID, target.ID, err)
		}
	}
	msg, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text, Entities: entities, LinkPreviewOptions: preview, ReplyMarkup: kb})
	if err != nil {
		log.Printf("telegram send: %v", err)
		return nil
	}
	log.Printf("telegram message sent chat=%d", chatID)
	if msg != nil {
		b.recordPage(chatID, msg.ID, text, kb, entities, push)
	}
	return msg
}

func addDomainCodeEntities(text string, existing []models.MessageEntity) []models.MessageEntity {
	entities := append([]models.MessageEntity(nil), existing...)
	entities = appendCodeEntities(text, visibleRuleTokenPattern.FindAllStringIndex(text, -1), entities, false)
	entities = appendCodeEntities(text, visibleDomainPattern.FindAllStringIndex(text, -1), entities, true)
	sort.SliceStable(entities, func(i, j int) bool {
		if entities[i].Offset != entities[j].Offset {
			return entities[i].Offset < entities[j].Offset
		}
		return entities[i].Length > entities[j].Length
	})
	return entities
}

func appendCodeEntities(text string, locations [][]int, entities []models.MessageEntity, validateDomain bool) []models.MessageEntity {
	for _, location := range locations {
		start, end := location[0], location[1]
		token := text[start:end]
		if validateDomain && (net.ParseIP(token) != nil || domainIsPartOfURLOrEmail(text, start)) {
			continue
		}
		offset := telegramTextLength(text[:start])
		length := telegramTextLength(token)
		if overlapsEntity(offset, length, entities) {
			continue
		}
		entities = append(entities, models.MessageEntity{
			Type:   models.MessageEntityTypeCode,
			Offset: offset,
			Length: length,
		})
	}
	return entities
}

func domainIsPartOfURLOrEmail(text string, start int) bool {
	if start > 0 && text[start-1] == '@' {
		return true
	}
	prefix := text[:start]
	separator := strings.LastIndexAny(prefix, " \t\r\n")
	currentToken := prefix[separator+1:]
	return strings.Contains(currentToken, "://")
}

func overlapsEntity(offset, length int, entities []models.MessageEntity) bool {
	end := offset + length
	for _, entity := range entities {
		entityEnd := entity.Offset + entity.Length
		if offset < entityEnd && entity.Offset < end {
			return true
		}
	}
	return false
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
			decision.ButtonText = "🟢 建议添加直连 · ⭐ 推荐"
		} else if geoKind == "proxy" {
			decision.ButtonAction = rules.Proxy
			decision.ButtonText = "🔴 建议添加代理 · ⭐ 推荐"
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
	decision.ButtonAction = opposite
	decision.ButtonText = switchButtonText(opposite, "（可选）")

	switch geoKind {
	case "direct":
		if effective.Action == rules.Direct {
			decision.Text = fmt.Sprintf("💡 建议：真实 IP 均在中国大陆，当前个人规则已经是%s，无需重复添加，建议保持现有规则。若确实需要调整，仍可切换为%s（不建议）。%s", current, oppositeText, matchNote)
			decision.ButtonText = switchButtonText(opposite, "（不建议）")
		} else {
			decision.Text = fmt.Sprintf("💡 建议：真实 IP 均在中国大陆，当前个人规则为%s，与当前地域判断相反。若访问确实需要直连，可切换为%s；CDN 位置可能变化，请结合实际访问确认。%s", current, actionText(rules.Direct), matchNote)
			decision.ButtonAction = rules.Direct
			decision.ButtonText = "🟢 切换为直连 · ⭐ 推荐"
		}
	case "proxy":
		if effective.Action == rules.Proxy {
			decision.Text = fmt.Sprintf("💡 建议：真实 IP 均在中国大陆以外，通常适合%s；当前个人规则已经是%s，无需重复添加，建议保持现有规则。若确实需要调整，可切换为%s（不建议）。%s", actionText(rules.Proxy), current, oppositeText, matchNote)
			decision.ButtonText = switchButtonText(opposite, "（不建议）")
		} else {
			decision.Text = fmt.Sprintf("💡 建议：真实 IP 均在中国大陆以外，通常适合%s；当前个人规则为%s。若访问确实需要代理，可切换为%s。CDN 位置可能变化，请结合实际访问确认。%s", actionText(rules.Proxy), current, actionText(rules.Proxy), matchNote)
			decision.ButtonAction = rules.Proxy
			decision.ButtonText = "🔴 切换为代理 · ⭐ 推荐"
		}
	case "both":
		decision.Text = fmt.Sprintf("💡 建议：当前域名同时解析到中国大陆和境外地址，可能存在 CDN 分流；当前个人规则为%s，建议先保持现有规则，不根据单次解析自动改分组。必要时可切换为%s。%s", current, oppositeText, matchNote)
	default:
		decision.Text = fmt.Sprintf("💡 建议：暂时无法根据真实 IP 判断分组；当前个人规则为%s，建议保持现有规则。必要时仍可切换为%s。%s", current, oppositeText, matchNote)
	}
	return decision
}

func switchButtonText(action rules.Action, suffix string) string {
	if action == rules.Direct {
		return "🟢 切换为直连" + suffix
	}
	return "🔴 切换为代理" + suffix
}

func queryActionButtons(decision suggestionDecision, hasDirect, hasProxy bool) []button {
	directText, proxyText := "🟢 添加为直连", "🔴 添加为代理"
	if hasProxy {
		directText = "🟢 覆写为个人直连"
	}
	if hasDirect {
		proxyText = "🔴 覆写为个人代理"
	}
	if decision.ButtonAction == rules.Direct {
		directText += " · ⭐ 推荐"
	} else if decision.ButtonAction == rules.Proxy {
		proxyText += " · ⭐ 推荐"
	}
	return []button{
		{Text: directText, Data: "suggest:direct"},
		{Text: proxyText, Data: "suggest:proxy"},
	}
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
