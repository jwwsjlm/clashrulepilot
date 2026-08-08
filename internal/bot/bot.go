package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"clashrulepilot/internal/app"
	"clashrulepilot/internal/config"
	"clashrulepilot/internal/domain"
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
	removed  map[int64]bool
	mu       sync.Mutex
}

type pending struct {
	Mode       string
	Domain     string
	Action     rules.Action
	Match      rules.Match
	Busy       bool
	Candidates []string
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
		removed:  make(map[int64]bool),
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
	b.ensureReplyKeyboardRemoved(ctx, item.Message.Chat.ID)
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
	if p == nil || p.Domain != "" || p.Mode == "" {
		return false
	}
	if strings.TrimSpace(text) == "" {
		b.send(ctx, chatID, "请输入域名，例如 example.com：", cancelMenu())
		return true
	}
	candidates, err := domain.Extract(text)
	if err != nil {
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
		p.Domain = domainName
		b.send(ctx, chatID, "请选择匹配方式：", keyboard([][]button{
			{{Text: "精确域名", Data: "match:exact"}, {Text: "包含子域名", Data: "match:suffix"}},
			{{Text: "取消", Data: "nav:cancel"}},
		}))
	case "query":
		b.clear(chatID)
		b.query(ctx, chatID, domainName)
	case "remove":
		b.clear(chatID)
		b.removeDomain(ctx, chatID, domainName)
	}
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
	b.sessions[chatID] = &pending{Mode: "add", Domain: domainName}
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
		pendingRule.Match = rules.Exact
	case "match:suffix":
		pendingRule.Match = rules.Suffix
	case "override:direct":
		pendingRule.Mode = "add"
		pendingRule.Action = rules.Direct
		pendingRule.Match = ""
	case "override:proxy":
		pendingRule.Mode = "add"
		pendingRule.Action = rules.Proxy
		pendingRule.Match = ""
	case "confirm:yes":
		if !b.markBusy(chatID) {
			return
		}
		result, err := b.service.AddRule(ctx, toRule(pendingRule, query.From.ID), true)
		b.clear(chatID)
		if err != nil {
			b.send(ctx, chatID, "提交失败："+err.Error(), homeMenu())
		} else {
			b.send(ctx, chatID, fmt.Sprintf("规则已移动并提交。\ncommit=%s", short(result.Commit)), homeMenu())
		}
		return
	default:
		log.Printf("telegram callback ignored: unknown data=%q", query.Data)
		b.send(ctx, chatID, "无法识别这个按钮，请返回主菜单。", homeMenu())
		return
	}

	if pendingRule.Action != "" && pendingRule.Match == "" {
		b.send(ctx, chatID, "请选择匹配方式：", keyboard([][]button{
			{{Text: "精确域名", Data: "match:exact"}, {Text: "包含子域名", Data: "match:suffix"}},
			{{Text: "取消", Data: "nav:cancel"}},
		}))
		return
	}
	if pendingRule.Action != "" && pendingRule.Match != "" {
		if !b.markBusy(chatID) {
			return
		}
		result, err := b.service.AddRule(ctx, toRule(pendingRule, query.From.ID), false)
		if conflict, ok := err.(*app.ConflictError); ok {
			b.setBusy(chatID, false)
			b.send(ctx, chatID, fmt.Sprintf("该规则已存在：%s/%s。是否移动到 %s？", conflict.Existing.Action, conflict.Existing.Match, pendingRule.Action), keyboard([][]button{
				{{Text: "确认移动", Data: "confirm:yes"}, {Text: "取消", Data: "nav:cancel"}},
			}))
			return
		}
		b.clear(chatID)
		if err != nil {
			b.send(ctx, chatID, "提交失败："+err.Error(), homeMenu())
		} else {
			b.send(ctx, chatID, fmt.Sprintf("规则已提交：%s %s/%s\ncommit=%s", pendingRule.Domain, pendingRule.Action, pendingRule.Match, short(result.Commit)), homeMenu())
		}
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
	domainName, err := domain.Normalize(strings.Join(parts[1:], " "))
	if err != nil {
		b.send(ctx, chatID, "域名无效："+err.Error(), homeMenu())
		return
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
	var geositeText []string
	hasDirect, hasProxy := false, false
	const maxDisplayedMatches = 20
	for _, match := range result.Upstream {
		if match.Action == ruleindex.GeoSite {
			geositeText = append(geositeText, fmt.Sprintf("命中 · %s · %s · 来源 %s", match.Kind, match.Pattern, match.Source))
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
	chinaSignal := "未检测到"
	if result.Network.GeoError != "" {
		chinaSignal = "检测失败"
	} else if result.Network.China {
		chinaSignal = fmt.Sprintf("检测到（%d 个地址已检查）", result.Network.ChinaChecked)
	}
	dnsText := fmt.Sprintf("A %d · AAAA %d", len(result.Network.A), len(result.Network.AAAA))
	if result.Network.DNSError != "" {
		dnsText = "解析失败"
	}
	root := result.Network.Registrable
	if root == "" {
		root = "未识别"
	}
	text := fmt.Sprintf("🔍 查询域名：%s\n👤 个人规则：%s\n📚 Aethersailor：%s\n🇨🇳 GEOSITE:CN：%s\n📡 中国大陆信号：%s\n🌐 DNS 解析：%s · 可注册域名 %s", domainName, personal, strings.Join(upstreamText, "\n  "), strings.Join(geositeText, "；"), chinaSignal, dnsText, root)
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
		b.sessions[chatID] = &pending{Mode: "query_override", Domain: domainName}
		b.mu.Unlock()
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
	for i, r := range store.Rules {
		if i >= 80 {
			fmt.Fprintf(&out, "\n… 其余 %d 条未显示", len(store.Rules)-i)
			break
		}
		fmt.Fprintf(&out, "%d. %s %s/%s\n", i+1, r.Domain, r.Action, r.Match)
	}
	b.send(ctx, chatID, out.String(), homeMenu())
}

func (b *Bot) sync(ctx context.Context, chatID int64) {
	if !b.service.IndexEnabled() && !b.service.SyncEnabled() {
		b.send(ctx, chatID, "本地查询索引和上游镜像都已关闭。", homeMenu())
		return
	}
	b.send(ctx, chatID, "正在同步 Aethersailor 与 GEOSITE:CN 本地索引，请稍候…", nil)
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
	updated := "从未"
	if !idx.UpdatedAt.IsZero() {
		updated = idx.UpdatedAt.In(b.cfg.Location).Format("2006-01-02 15:04:05")
	}
	lastError := "无"
	if idx.LastError != "" {
		lastError = idx.LastError
	}
	b.send(ctx, chatID, fmt.Sprintf("ClashRulePilot 运行状态\n发布仓库：%s (%s)\n个人规则：%d 条\n本地查询索引：%t / 已加载=%t\n规则文件：%d 个（仅保留最新快照）\n索引规则：直连 %d · 代理 %d · 分类 %d · GEOSITE:CN %d\n索引更新时间：%s\n索引异常：%s\n上游公开镜像：%t", b.cfg.RuleRepoProject, b.cfg.RuleRepoProvider, len(store.Rules), idx.Enabled, idx.Loaded, idx.Sources, idx.Direct, idx.Proxy, idx.Category, idx.GeoSite, updated, lastError, b.service.SyncEnabled()), homeMenu())
}

func (b *Bot) helpText() string {
	return "ClashRulePilot 使用帮助\n\n点击消息中的 Inline 按钮即可操作。支持发送纯域名、URL、域名:端口或完整 OpenClash/Mihomo 日志。添加规则时再选择精确匹配或包含子域名。\n\n命令仍然兼容：/query、/add、/remove、/list、/sync、/status、/help"
}

func (b *Bot) welcomeText() string {
	return "ClashRulePilot\n\n请选择要执行的操作："
}

func (b *Bot) repo(ctx context.Context, chatID int64) {
	b.send(ctx, chatID, fmt.Sprintf("规则仓库\n%s\n\n个人覆写：\n%s", b.service.RepoWebURL(), b.service.RepoRawURL("openclash/personal-overwrite.ini")), homeMenu())
}

func (b *Bot) clear(id int64) { b.mu.Lock(); delete(b.sessions, id); b.mu.Unlock() }
func (b *Bot) ensureReplyKeyboardRemoved(ctx context.Context, chatID int64) {
	b.mu.Lock()
	if b.removed[chatID] {
		b.mu.Unlock()
		return
	}
	b.removed[chatID] = true
	b.mu.Unlock()
	b.send(ctx, chatID, "✅ 已切换为聊天消息内按钮。", &models.ReplyKeyboardRemove{RemoveKeyboard: true})
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
	if m == rules.Exact {
		return "精确"
	}
	return "包含子域名"
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
