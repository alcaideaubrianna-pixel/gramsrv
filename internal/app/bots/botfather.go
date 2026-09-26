package bots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	telegramloginapp "telesrv/internal/app/telegramlogin"
	"telesrv/internal/branding"
	"telesrv/internal/domain"
	"telesrv/internal/store"
)

// BotFather 对话状态机：用户发给 BotFather 的每条私聊消息经 app/messages 的
// responder hook 进入 OnPrivateMessage；用户消息已先行入库，这里只负责生成并
// 写入 BotFather 的回复（完整 SendPrivateText 链路：双盒+事件+outbox 推送）。

const (
	botFatherCmdNewBot         = "newbot"
	botFatherCmdToken          = "token"
	botFatherCmdRevoke         = "revoke"
	botFatherCmdSetName        = "setname"
	botFatherCmdSetDescription = "setdescription"
	botFatherCmdSetAbout       = "setabouttext"
	botFatherCmdSetCommands    = "setcommands"
	botFatherCmdSetInline      = "setinline"
	botFatherCmdSetInlineGeo   = "setinlinegeo"
	botFatherCmdSetInlineFB    = "setinlinefeedback"
	botFatherCmdSetJoinGroups  = "setjoingroups"
	botFatherCmdSetPrivacy     = "setprivacy"
	botFatherCmdSetLogin       = "setlogin"
	botFatherCmdLoginInfo      = "logininfo"
	botFatherCmdResetLogin     = "resetloginsecret"
	botFatherCmdDone           = "done"

	botFatherStepName     = "name"
	botFatherStepUsername = "username"
	botFatherStepChoose   = "choose"
	botFatherStepValue    = "value"

	botFatherDraftBotID       = "bot_id"
	botFatherDraftBotUsername = "bot_username"

	maxTelegramLoginCommandsPerMessage = 32
)

func botFatherHelpText() string {
	return `我可以帮助你创建和管理 ` + branding.ProductName() + ` 机器人。

你可以发送以下命令控制我：

/newbot - 创建机器人
/mybots - 查看你的机器人列表
/token - 查看机器人 Token
/revoke - 撤销机器人 Token
/setname - 修改机器人名称
/setdescription - 修改机器人简介
/setabouttext - 修改机器人资料说明
/setcommands - 修改机器人命令列表
/setinline - 开关 Inline 模式
/setinlinegeo - 开关 Inline 位置请求
/setinlinefeedback - 修改 Inline 反馈设置
/setjoingroups - 设置机器人是否可以加入群组
/setprivacy - 设置机器人群组隐私模式
/setlogin - 配置 Telegram Login 允许的 URL 和签名
/logininfo - 查看机器人 Telegram Login 配置
/resetloginsecret - 轮换 OIDC 客户端密钥
/done - 完成当前 Telegram Login 配置
/cancel - 取消当前操作
/help - 显示此帮助`
}

// botReply 是内置 service bot 的一条回复。ReplyMarkup 为可选 inline keyboard
// 快照（@verifybot 的按钮式对话使用）；落库前经 domain.ValidateReplyMarkup 校验。
type botReply struct {
	Text        string
	Entities    []domain.MessageEntity
	ReplyMarkup *domain.MessageReplyMarkup
	Media       *domain.MessageMedia
}

// HandlesBot 报告该收件人是否为内置应答 bot（messages.BotResponder 实现）。
func (s *Service) HandlesBot(botUserID int64) bool {
	if s == nil {
		return false
	}
	if s.premium != nil && botUserID == s.premium.BotUserID() {
		return true
	}
	switch botUserID {
	case domain.BotFatherUserID, domain.StickersBotUserID, domain.ChatBotUserID,
		domain.VerifyBotUserID, domain.VerifierBotUserID:
		return true
	default:
		return false
	}
}

// OnPrivateMessage 处理投递给内置 bot 的私聊消息（messages.BotResponder 实现）。
// msg 是 bot 视角的收件 box 行。回复异步生成（不占用户 sendMessage 的 RPC
// goroutine——官方 bot 回复本就异步到达），失败只记日志，绝不影响用户消息本身。
func (s *Service) OnPrivateMessage(ctx context.Context, botUserID int64, msg domain.Message, session domain.ClientSessionMetadata) {
	if s == nil || s.messages == nil || !s.HandlesBot(botUserID) {
		return
	}
	userID := msg.From.ID
	if msg.From.Type != domain.PeerTypeUser || userID == 0 || userID == botUserID {
		return
	}
	switch botUserID {
	case domain.BotFatherUserID:
		go s.respondAsBotFather(userID, msg.Body)
	case domain.StickersBotUserID:
		go s.respondAsStickers(userID, msg)
	case domain.ChatBotUserID:
		go s.respondAsChatBot(userID, msg)
	case domain.VerifyBotUserID:
		go s.respondAsVerify(userID, msg)
	case domain.VerifierBotUserID:
		go s.respondAsVerifier(userID, msg)
	default:
		if s.premium != nil && botUserID == s.premium.BotUserID() {
			go s.respondAsPremium(botUserID, userID, msg, session)
		}
	}
}

// respondAsBotFather 生成并写入 BotFather 回复（OnPrivateMessage 在 goroutine 内调用）。
// 按用户取条带锁串行：状态机 Get→modify→Upsert/Delete 的 RMW 因此原子、回复保序，
// 不同用户并发不受影响。ctx 用 Background（脱离已返回的用户 RPC），限较长超时。
func (s *Service) respondAsBotFather(userID int64, body string) {
	mu := s.serviceBotReplyLock(domain.BotFatherUserID, userID)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reply := s.handleBotFather(ctx, userID, body)
	s.sendServiceBotReply(ctx, domain.BotFatherUserID, userID, reply)
}

func (s *Service) serviceBotReplyLock(botUserID, userID int64) *sync.Mutex {
	key := uint64(userID) ^ (uint64(botUserID) * 11400714819323198485)
	return &s.replyLocks[key%replyLockStripes]
}

func (s *Service) sendServiceBotReply(ctx context.Context, botUserID, userID int64, reply botReply) {
	_, _ = s.sendServiceBotReplyResult(ctx, botUserID, userID, reply)
}

func (s *Service) serviceBotRecipientBlocked(ctx context.Context, botUserID, userID int64) bool {
	if s == nil || s.blocker == nil {
		return false
	}
	blocked, err := s.blocker.IsBlocked(ctx, userID, botUserID)
	if err != nil {
		s.log.Warn("service bot: check block", zap.Int64("bot_user_id", botUserID), zap.Int64("user_id", userID), zap.Error(err))
		return false
	}
	return blocked
}

func (s *Service) sendServiceBotReplyResult(ctx context.Context, botUserID, userID int64, reply botReply) (domain.SendPrivateTextResult, bool) {
	if s == nil || s.messages == nil || (reply.Text == "" && reply.Media.IsZero()) {
		return domain.SendPrivateTextResult{}, false
	}
	markup := reply.ReplyMarkup
	if err := domain.ValidateReplyMarkup(markup); err != nil {
		// 键盘校验必须先于落库（I9）：结构非法的 markup 绝不写库，但正文仍然发出
		// ——用户至少收到提示文本，不会因为一颗坏按钮而完全失联。
		s.log.Error("service bot: invalid reply markup",
			zap.Int64("bot_user_id", botUserID), zap.Int64("user_id", userID), zap.Error(err))
		markup = nil
	}
	if markup.IsZero() {
		markup = nil
	}
	res, err := s.messages.SendPrivateText(ctx, domain.SendPrivateTextRequest{
		SenderUserID:     botUserID,
		RecipientUserID:  userID,
		RandomID:         s.botReplyRandomID(),
		Message:          reply.Text,
		Entities:         serviceBotReplyEntities(reply.Text, reply.Entities),
		Media:            reply.Media,
		ReplyMarkup:      markup,
		Date:             int(s.now().Unix()),
		RecipientBlocked: s.serviceBotRecipientBlocked(ctx, botUserID, userID),
	})
	if err != nil {
		s.log.Error("service bot: send reply", zap.Int64("bot_user_id", botUserID), zap.Int64("user_id", userID), zap.Error(err))
		return domain.SendPrivateTextResult{}, false
	}
	return res, true
}

// botReplyRandomID 为服务端回复构造非零幂等键（(sender, random_id) 唯一索引）。
// 所有服务 bot 回复按各自 sender 命名空间唯一——用
// crypto/rand 取 64 位随机数（碰撞概率可忽略），熵源失败时退化为纳秒+单调序列。
func (s *Service) botReplyRandomID() int64 {
	if v, err := randomInt64(); err == nil && v != 0 {
		return v
	}
	v := s.now().UnixNano() + s.replySeq.Add(1)
	if v == 0 {
		v = 1
	}
	return v
}

// botFatherGlobalCommands 是任何状态下都优先按命令处理的全局命令。其余以 "/"
// 开头的文本（如 /empty、或粘贴的 "/start - Begin" 命令列表首行）在收值步骤里
// 必须作为原始内容透传给状态机，否则 /setcommands 的 /empty 永不可达、且首行
// 带斜杠的命令列表会被截成命令名 "start" 静默销毁整个流程。
var botFatherGlobalCommands = map[string]bool{
	"start": true, "help": true, "cancel": true, botFatherCmdDone: true,
	botFatherCmdNewBot: true, "mybots": true,
	botFatherCmdToken: true, botFatherCmdRevoke: true,
	botFatherCmdSetName: true, botFatherCmdSetDescription: true, botFatherCmdSetAbout: true,
	botFatherCmdSetCommands: true, botFatherCmdSetInline: true, botFatherCmdSetInlineGeo: true,
	botFatherCmdSetInlineFB: true, botFatherCmdSetJoinGroups: true, botFatherCmdSetPrivacy: true,
	botFatherCmdSetLogin: true, botFatherCmdLoginInfo: true, botFatherCmdResetLogin: true,
}

func (s *Service) handleBotFather(ctx context.Context, userID int64, text string) botReply {
	text = strings.TrimSpace(text)
	state, found, err := s.bots.GetBotChatState(ctx, domain.BotFatherUserID, userID)
	if err != nil {
		s.log.Error("botfather: get chat state", zap.Int64("user_id", userID), zap.Error(err))
		return internalReply()
	}
	// 命令拦截：仅当不在收值步骤、或文本是已知全局命令时，才走命令分发。收值步骤
	// 下的非全局 "/..." 文本（/empty、命令列表首行）必须当原始值透传给状态机。
	if cmd, ok := parseBotCommand(text); ok {
		inValueStep := found && state.Step == botFatherStepValue
		if !inValueStep || botFatherGlobalCommands[cmd] {
			return s.handleBotFatherCommand(ctx, userID, cmd)
		}
	}
	if text == "" {
		// 空白文本 / 贴纸 / 无 caption 媒体：有活动状态时回当前步骤提示，
		// 无状态保持沉默（避免对任意非文本消息刷屏）。
		if !found {
			return botReply{}
		}
		return s.stepPrompt(state)
	}
	if !found {
		return botReply{Text: "我只能帮助你创建和管理机器人，请发送 /help 查看命令列表。"}
	}
	switch {
	case state.Command == botFatherCmdNewBot && state.Step == botFatherStepName:
		return s.handleNewBotName(ctx, state, text)
	case state.Command == botFatherCmdNewBot && state.Step == botFatherStepUsername:
		return s.handleNewBotUsername(ctx, state, text)
	case state.Step == botFatherStepChoose:
		return s.handleChooseBot(ctx, state, text)
	case state.Step == botFatherStepValue:
		return s.handleSetValue(ctx, state, text)
	default:
		// 不可达的脏状态：清掉重来，避免用户被卡死。
		_ = s.bots.DeleteBotChatState(ctx, domain.BotFatherUserID, userID)
		return botReply{Text: "出现问题，当前操作状态已丢失。请发送 /help 查看命令列表。"}
	}
}

// pickerCommands 是「先选 bot」的命令集（choose step 后按命令分流）。
var pickerPrompts = map[string]string{
	botFatherCmdToken:          "请选择要生成 Token 的机器人，请发送机器人的用户名：",
	botFatherCmdRevoke:         "请选择要撤销 Token 的机器人，请发送机器人的用户名：",
	botFatherCmdSetName:        "请选择要修改名称的机器人，请发送机器人的用户名：",
	botFatherCmdSetDescription: "请选择要修改简介的机器人，请发送机器人的用户名：",
	botFatherCmdSetAbout:       "请选择要修改资料说明的机器人，请发送机器人的用户名：",
	botFatherCmdSetCommands:    "请选择要修改命令列表的机器人，请发送机器人的用户名：",
	botFatherCmdSetInline:      "请选择要配置 Inline 模式的机器人，请发送机器人的用户名：",
	botFatherCmdSetInlineGeo:   "请选择要配置 Inline 位置请求的机器人，请发送机器人的用户名：",
	botFatherCmdSetJoinGroups:  "请选择要配置群组加入权限的机器人，请发送机器人的用户名：",
	botFatherCmdSetPrivacy:     "请选择要配置群组隐私模式的机器人，请发送机器人的用户名：",
	botFatherCmdSetLogin:       "请选择要配置 Telegram Login 的机器人，请发送机器人的用户名：",
	botFatherCmdLoginInfo:      "请选择要查看 Telegram Login 配置的机器人：",
	botFatherCmdResetLogin:     "请选择要轮换 OIDC 客户端密钥的机器人：",
}

// startBotPicker 列出 owner 的 bot 并进入 choose step（所有需先选 bot 的命令共用）。
func (s *Service) startBotPicker(ctx context.Context, userID int64, cmd string) botReply {
	usernames, err := s.ownedBotUsernames(ctx, userID)
	if err != nil {
		s.log.Error("botfather: list bots", zap.Int64("user_id", userID), zap.Error(err))
		return internalReply()
	}
	if len(usernames) == 0 {
		return botReply{Text: "你还没有机器人，请使用 /newbot 创建。"}
	}
	if err := s.bots.UpsertBotChatState(ctx, domain.BotChatState{
		BotUserID: domain.BotFatherUserID,
		UserID:    userID,
		Command:   cmd,
		Step:      botFatherStepChoose,
	}); err != nil {
		s.log.Error("botfather: save chat state", zap.Int64("user_id", userID), zap.Error(err))
		return internalReply()
	}
	return botReply{Text: pickerPrompts[cmd] + "\n\n@" + strings.Join(usernames, "\n@")}
}

// stepPrompt 返回当前对话步骤的引导文案（空输入兜底用）。
func (s *Service) stepPrompt(state domain.BotChatState) botReply {
	switch {
	case state.Command == botFatherCmdNewBot && state.Step == botFatherStepName:
		return botReply{Text: "请选择机器人名称，或发送 /cancel。"}
	case state.Command == botFatherCmdNewBot && state.Step == botFatherStepUsername:
		return botReply{Text: "请发送机器人用户名。用户名必须以 `bot` 结尾，也可以发送 /cancel。"}
	case state.Step == botFatherStepChoose:
		return botReply{Text: "请发送你拥有的机器人用户名，或发送 /cancel。"}
	case state.Step == botFatherStepValue:
		return botReply{Text: valuePrompt(state.Command, state.Draft[botFatherDraftBotUsername])}
	default:
		return botReply{Text: "请发送 /help 查看命令列表。"}
	}
}

func (s *Service) handleBotFatherCommand(ctx context.Context, userID int64, cmd string) botReply {
	switch cmd {
	case "start", "help":
		_ = s.bots.DeleteBotChatState(ctx, domain.BotFatherUserID, userID)
		return botReply{Text: botFatherHelpText()}
	case "cancel":
		state, found, err := s.bots.GetBotChatState(ctx, domain.BotFatherUserID, userID)
		if err != nil {
			s.log.Error("botfather: get chat state", zap.Int64("user_id", userID), zap.Error(err))
			return internalReply()
		}
		if !found {
			return botReply{Text: "当前没有可取消的操作。"}
		}
		if err := s.bots.DeleteBotChatState(ctx, domain.BotFatherUserID, userID); err != nil {
			s.log.Error("botfather: delete chat state", zap.Int64("user_id", userID), zap.Error(err))
			return internalReply()
		}
		if state.Command == botFatherCmdSetLogin && state.Step == botFatherStepValue {
			return botReply{Text: "Telegram Login 配置已关闭，之前已经应用的更改会保留。"}
		}
		return botReply{Text: "操作已取消。需要其他帮助吗？请发送 /help 查看命令列表。"}
	case botFatherCmdDone:
		return s.finishTelegramLoginConfiguration(ctx, userID)
	case botFatherCmdNewBot:
		count, err := s.bots.CountBotsByOwner(ctx, userID)
		if err != nil {
			s.log.Error("botfather: count bots", zap.Int64("user_id", userID), zap.Error(err))
			return internalReply()
		}
		if count >= domain.MaxBotsPerOwner {
			return botReply{Text: fmt.Sprintf("无法继续操作，每个账号最多只能创建 %d 个机器人。", domain.MaxBotsPerOwner)}
		}
		if err := s.bots.UpsertBotChatState(ctx, domain.BotChatState{
			BotUserID: domain.BotFatherUserID,
			UserID:    userID,
			Command:   botFatherCmdNewBot,
			Step:      botFatherStepName,
		}); err != nil {
			s.log.Error("botfather: save chat state", zap.Int64("user_id", userID), zap.Error(err))
			return internalReply()
		}
		return botReply{Text: "好的，我们来创建一个新机器人。请先选择机器人名称。"}
	case "mybots":
		usernames, err := s.ownedBotUsernames(ctx, userID)
		if err != nil {
			s.log.Error("botfather: list bots", zap.Int64("user_id", userID), zap.Error(err))
			return internalReply()
		}
		if len(usernames) == 0 {
			return botReply{Text: "你还没有机器人，请使用 /newbot 创建。"}
		}
		return botReply{Text: "这是你的机器人列表：\n\n@" + strings.Join(usernames, "\n@")}
	case botFatherCmdToken, botFatherCmdRevoke,
		botFatherCmdSetName, botFatherCmdSetDescription, botFatherCmdSetAbout,
		botFatherCmdSetCommands, botFatherCmdSetInline, botFatherCmdSetInlineGeo,
		botFatherCmdSetJoinGroups, botFatherCmdSetPrivacy,
		botFatherCmdSetLogin, botFatherCmdLoginInfo, botFatherCmdResetLogin:
		return s.startBotPicker(ctx, userID, cmd)
	case botFatherCmdSetInlineFB:
		_ = s.bots.DeleteBotChatState(ctx, domain.BotFatherUserID, userID)
		return botReply{Text: "Inline 反馈设置暂不支持，请使用 /setinline 开启或关闭 Inline 模式。"}
	default:
		return botReply{Text: "无法识别此命令，请发送 /help 查看命令列表。"}
	}
}

// valuePrompts 是选中 bot 后、value step 的收值提示（按命令）。
func valuePrompt(cmd, username string) string {
	switch cmd {
	case botFatherCmdSetName:
		return fmt.Sprintf("好的，请发送 @%s 的新名称。", username)
	case botFatherCmdSetDescription:
		return fmt.Sprintf("好的，请发送 @%s 的新简介。用户开始与机器人聊天前，会在机器人资料页看到这段简介。", username)
	case botFatherCmdSetAbout:
		return fmt.Sprintf("好的，请发送 @%s 的新资料说明。用户会在机器人资料页看到这段文字；分享机器人时，这段文字也会与机器人链接一同发送。", username)
	case botFatherCmdSetCommands:
		return fmt.Sprintf("好的，请发送 @%s 的命令列表。请使用以下格式：\n\ncommand1 - 描述\ncommand2 - 另一条描述\n\n发送 /empty 清空命令列表。", username)
	case botFatherCmdSetInline:
		return fmt.Sprintf("这将为 @%s 启用 Inline 查询。请发送用户输入机器人用户名后将看到的占位提示文字，或发送 /empty 关闭 Inline 模式。", username)
	case botFatherCmdSetInlineGeo:
		return fmt.Sprintf("发送 'enable' 允许 @%s 在 Inline 查询中接收位置，发送 'disable' 关闭此功能。", username)
	case botFatherCmdSetJoinGroups:
		return fmt.Sprintf("发送 'enable' 允许将 @%s 添加到群组，发送 'disable' 禁止添加。", username)
	case botFatherCmdSetPrivacy:
		return fmt.Sprintf("发送 'enable' 为 @%s 开启群组隐私模式（机器人只会收到命令和回复），发送 'disable' 让它接收所有群组消息。", username)
	case botFatherCmdSetLogin:
		return telegramLoginConfigurationPrompt(username)
	default:
		return "请发送新值，或发送 /cancel。"
	}
}

func (s *Service) handleNewBotName(ctx context.Context, state domain.BotChatState, name string) botReply {
	if name == "" || len([]rune(name)) > domain.MaxBotNameLength {
		return botReply{Text: fmt.Sprintf("机器人名称长度必须为 1-%d 个字符，请换一个名称。", domain.MaxBotNameLength)}
	}
	state.Step = botFatherStepUsername
	if state.Draft == nil {
		state.Draft = map[string]string{}
	}
	state.Draft["name"] = name
	if err := s.bots.UpsertBotChatState(ctx, state); err != nil {
		s.log.Error("botfather: save chat state", zap.Int64("user_id", state.UserID), zap.Error(err))
		return internalReply()
	}
	return botReply{Text: "很好。现在请为机器人选择用户名。用户名必须以 `bot` 结尾，例如：TetrisBot 或 tetris_bot。"}
}

func (s *Service) handleNewBotUsername(ctx context.Context, state domain.BotChatState, username string) botReply {
	var effects store.DeliveryEffectsBuilder[store.BotLifecycleDeliverySnapshot]
	if s.hooks != nil {
		effects = s.hooks.BotLifecycleDeliveryEffects(ctx)
	}
	u, token, err := s.CreateBotWithDelivery(ctx, state.UserID, state.Draft["name"], username, effects)
	switch {
	case errors.Is(err, domain.ErrBotUsernameInvalid):
		return botReply{Text: "用户名无效。机器人用户名长度必须为 5-32 个字符，以字母开头，只能包含英文字母、数字和下划线，并以 bot 结尾（例如 tetris_bot）。"}
	case errors.Is(err, domain.ErrUsernameOccupied):
		return botReply{Text: "该用户名已被占用，请换一个。"}
	case errors.Is(err, domain.ErrBotsTooMany):
		_ = s.bots.DeleteBotChatState(ctx, domain.BotFatherUserID, state.UserID)
		return botReply{Text: fmt.Sprintf("无法继续操作，每个账号最多只能创建 %d 个机器人。", domain.MaxBotsPerOwner)}
	case errors.Is(err, domain.ErrBotNameInvalid):
		// name 步已校验，这里只可能是脏状态；重新走 name 步。
		state.Step = botFatherStepName
		if err := s.bots.UpsertBotChatState(ctx, state); err != nil {
			s.log.Error("botfather: save chat state", zap.Int64("user_id", state.UserID), zap.Error(err))
			return internalReply()
		}
		return botReply{Text: "请先选择机器人名称。"}
	case err != nil:
		s.log.Error("botfather: create bot", zap.Int64("user_id", state.UserID), zap.Error(err))
		return internalReply()
	}
	if err := s.bots.DeleteBotChatState(ctx, domain.BotFatherUserID, state.UserID); err != nil {
		s.log.Error("botfather: delete chat state", zap.Int64("user_id", state.UserID), zap.Error(err))
	}
	head := fmt.Sprintf("完成！恭喜你创建了新机器人，可在 %s 找到它。\n\n使用以下 Token 访问 HTTP API：\n", s.publicURL(u.Username))
	return tokenReply(head, token, "\n\n请妥善保管 Token 并安全存储，任何人都可以使用它控制你的机器人。")
}

func (s *Service) handleChooseBot(ctx context.Context, state domain.BotChatState, text string) botReply {
	username := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(text, "@")))
	profiles, err := s.ownedBots(ctx, state.UserID)
	if err != nil {
		s.log.Error("botfather: list bots", zap.Int64("user_id", state.UserID), zap.Error(err))
		return internalReply()
	}
	var chosen *domain.User
	for i := range profiles {
		if strings.ToLower(profiles[i].user.Username) == username {
			chosen = &profiles[i].user
			break
		}
	}
	if chosen == nil {
		return botReply{Text: "找不到属于你的机器人，请发送你拥有的机器人用户名，或发送 /cancel。"}
	}
	switch state.Command {
	case botFatherCmdToken:
		defer s.clearState(ctx, state.UserID)
		profile, found, err := s.bots.GetBot(ctx, chosen.ID)
		if err != nil || !found || profile.TokenSecret == "" {
			if err != nil {
				s.log.Error("botfather: get bot", zap.Int64("bot_user_id", chosen.ID), zap.Error(err))
			}
			return internalReply()
		}
		head := fmt.Sprintf("你可以使用以下 Token 访问 @%s 的 HTTP API：\n", chosen.Username)
		return tokenReply(head, domain.FormatBotToken(chosen.ID, profile.TokenSecret), "\n\n请妥善保管 Token 并安全存储，任何人都可以使用它控制你的机器人。")
	case botFatherCmdRevoke:
		defer s.clearState(ctx, state.UserID)
		token, err := s.RevokeBotToken(ctx, state.UserID, chosen.ID)
		switch {
		case errors.Is(err, domain.ErrBotSessionsNotRevoked):
			// token 已换（旧 token 不可再登录），但未能终止已建立的 session——
			// 诚实告知用户重试，不谎称已止血。
			head := fmt.Sprintf("@%s 的 Token 已更换，旧 Token 无法再登录。但无法终止已经登录的会话，请再次执行 /revoke，确保这些会话被切断。新 Token：\n", chosen.Username)
			return tokenReply(head, token, "\n\n请妥善保管 Token 并安全存储，任何人都可以使用它控制你的机器人。")
		case err != nil:
			s.log.Error("botfather: revoke token", zap.Int64("bot_user_id", chosen.ID), zap.Error(err))
			return internalReply()
		}
		head := fmt.Sprintf("@%s 的 Token 已撤销，旧 Token 将立即失效。新 Token：\n", chosen.Username)
		return tokenReply(head, token, "\n\n请妥善保管 Token 并安全存储，任何人都可以使用它控制你的机器人。")
	case botFatherCmdLoginInfo:
		defer s.clearState(ctx, state.UserID)
		if s.telegramLogin == nil {
			return botReply{Text: "此服务器未启用 Telegram Login。"}
		}
		configuration, found, err := s.telegramLogin.ClientConfiguration(ctx, chosen.ID)
		if err != nil {
			s.log.Error("botfather: get telegram login configuration", zap.Int64("bot_user_id", chosen.ID), zap.Error(err))
			return internalReply()
		}
		if !found {
			return botReply{Text: fmt.Sprintf("@%s 尚未配置 Telegram Login，请使用 /setlogin 创建配置。", chosen.Username)}
		}
		return botReply{Text: formatTelegramLoginConfiguration(chosen.Username, configuration)}
	case botFatherCmdResetLogin:
		defer s.clearState(ctx, state.UserID)
		if s.telegramLogin == nil {
			return botReply{Text: "此服务器未启用 Telegram Login。"}
		}
		credentials, err := s.telegramLogin.RotateClientSecret(ctx, chosen.ID)
		if errors.Is(err, domain.ErrTelegramLoginClientInvalid) {
			return botReply{Text: fmt.Sprintf("@%s 尚未配置 Telegram Login，请先使用 /setlogin。", chosen.Username)}
		}
		if err != nil {
			s.log.Error("botfather: rotate telegram login secret", zap.Int64("bot_user_id", chosen.ID), zap.Error(err))
			return internalReply()
		}
		head := fmt.Sprintf("@%s 的旧 OIDC 客户端密钥已失效。请保存新的密钥，它只会显示一次：\n", chosen.Username)
		return tokenReply(head, credentials.Secret, "\n\nClient ID: "+credentials.Client.ClientID)
	case botFatherCmdSetLogin:
		if s.telegramLogin == nil {
			s.clearState(ctx, state.UserID)
			return botReply{Text: "此服务器未启用 Telegram Login。"}
		}
		credentials, created, err := s.telegramLogin.EnsureClient(ctx, chosen.ID)
		if err != nil {
			s.log.Error("botfather: ensure telegram login client", zap.Int64("bot_user_id", chosen.ID), zap.Error(err))
			return internalReply()
		}
		state.Step = botFatherStepValue
		if state.Draft == nil {
			state.Draft = map[string]string{}
		}
		state.Draft[botFatherDraftBotID] = strconv.FormatInt(chosen.ID, 10)
		state.Draft[botFatherDraftBotUsername] = chosen.Username
		if err := s.bots.UpsertBotChatState(ctx, state); err != nil {
			s.log.Error("botfather: save telegram login state", zap.Int64("user_id", state.UserID), zap.Error(err))
			return internalReply()
		}
		prompt := telegramLoginConfigurationPrompt(chosen.Username)
		if !created {
			return botReply{Text: fmt.Sprintf("@%s 的 Telegram Login 客户端 %s 已准备就绪。\n\n%s", chosen.Username, credentials.Client.ClientID, prompt)}
		}
		head := fmt.Sprintf("@%s 的 Telegram Login 已启用。\n客户端 ID：%s\n请保存此客户端密钥，它只会显示一次：\n", chosen.Username, credentials.Client.ClientID)
		return tokenReply(head, credentials.Secret, "\n\n"+prompt)
	case botFatherCmdSetName, botFatherCmdSetDescription, botFatherCmdSetAbout,
		botFatherCmdSetCommands, botFatherCmdSetInline, botFatherCmdSetInlineGeo,
		botFatherCmdSetJoinGroups, botFatherCmdSetPrivacy:
		// 选中 bot 后进入收值 step，把目标 bot 暂存进 Draft。
		state.Step = botFatherStepValue
		if state.Draft == nil {
			state.Draft = map[string]string{}
		}
		state.Draft[botFatherDraftBotID] = strconv.FormatInt(chosen.ID, 10)
		state.Draft[botFatherDraftBotUsername] = chosen.Username
		if err := s.bots.UpsertBotChatState(ctx, state); err != nil {
			s.log.Error("botfather: save chat state", zap.Int64("user_id", state.UserID), zap.Error(err))
			return internalReply()
		}
		return botReply{Text: valuePrompt(state.Command, chosen.Username)}
	default:
		s.clearState(ctx, state.UserID)
		return internalReply()
	}
}

// handleSetValue 处理选中 bot 后的收值步骤（/setname 等）。
func (s *Service) handleSetValue(ctx context.Context, state domain.BotChatState, text string) botReply {
	botID, _ := strconv.ParseInt(state.Draft[botFatherDraftBotID], 10, 64)
	username := state.Draft[botFatherDraftBotUsername]
	if botID == 0 {
		s.clearState(ctx, state.UserID)
		return botReply{Text: "出了点问题，我忘记了正在编辑哪个机器人。请发送 /help。"}
	}
	// 防御性复核 owner（状态是服务端存的，正常已是 owned bot）。
	if owns, err := s.OwnsBot(ctx, state.UserID, botID); err != nil {
		s.log.Error("botfather: owns bot", zap.Int64("bot_user_id", botID), zap.Error(err))
		return internalReply()
	} else if !owns {
		s.clearState(ctx, state.UserID)
		return botReply{Text: "该机器人已不可用。"}
	}

	var (
		reply botReply
		err   error
	)
	var infoEffects store.DeliveryEffectsBuilder[store.UserAudienceDeliverySnapshot]
	if s.hooks != nil && (state.Command == botFatherCmdSetName || state.Command == botFatherCmdSetDescription || state.Command == botFatherCmdSetAbout) {
		infoEffects = s.hooks.BotInfoDeliveryEffects(ctx)
	}
	switch state.Command {
	case botFatherCmdSetName:
		_, err = s.SetBotInfoWithDelivery(ctx, botID, domain.BotInfoUpdate{SetName: true, Name: text}, infoEffects)
		reply = okReply(err, fmt.Sprintf("成功！@%s 的名称已更新。", username), "名称无效，请换一个名称。")
	case botFatherCmdSetDescription:
		_, err = s.SetBotInfoWithDelivery(ctx, botID, domain.BotInfoUpdate{SetDescription: true, Description: text}, infoEffects)
		reply = okReply(err, "成功！简介已更新。", "简介太长了。")
	case botFatherCmdSetAbout:
		_, err = s.SetBotInfoWithDelivery(ctx, botID, domain.BotInfoUpdate{SetAbout: true, About: text}, infoEffects)
		reply = okReply(err, "成功！资料说明已更新。", "资料说明太长了。")
	case botFatherCmdSetCommands:
		reply, err = s.applySetCommands(ctx, botID, text)
	case botFatherCmdSetInline:
		reply, err = s.applySetInline(ctx, botID, text)
	case botFatherCmdSetInlineGeo:
		reply, err = s.applySetInlineGeo(ctx, botID, text)
	case botFatherCmdSetJoinGroups:
		reply, err = s.applyToggle(ctx, botID, text, true)
	case botFatherCmdSetPrivacy:
		reply, err = s.applyToggle(ctx, botID, text, false)
	case botFatherCmdSetLogin:
		return s.handleTelegramLoginConfigurationInput(ctx, state, botID, username, text)
	default:
		s.clearState(ctx, state.UserID)
		return internalReply()
	}
	if err != nil {
		// 校验类错误已转成提示文案；非校验错误内部已记日志。保留 state 让用户重试。
		if reply.Text == "" {
			return internalReply()
		}
		return reply
	}
	s.clearState(ctx, state.UserID)
	return reply
}

// applySetCommands 解析多行 "command - Description"（/empty 清空）并写入。
func (s *Service) applySetCommands(ctx context.Context, botID int64, text string) (botReply, error) {
	var commands []domain.BotCommand
	if strings.TrimSpace(text) != "/empty" {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			cmd, desc, ok := strings.Cut(line, "-")
			if !ok {
				return botReply{Text: "格式无效，每行必须为：command - 描述。请重试或发送 /cancel。"}, domain.ErrBotCommandInvalid
			}
			commands = append(commands, domain.BotCommand{
				Command:     strings.TrimSpace(cmd),
				Description: strings.TrimSpace(desc),
			})
		}
	}
	if _, err := s.SetBotCommands(ctx, botID, commands); err != nil {
		return botReply{Text: "命令列表无效。每条命令必须为 1-32 个字符，只能包含字母、数字和下划线，且描述不能为空。请重试或发送 /cancel。"}, err
	}
	return botReply{Text: "成功！命令列表已更新。请发送 /help 查看其他操作。"}, nil
}

// applySetInline 写入 inline placeholder；/empty 清空并关闭 inline mode。
func (s *Service) applySetInline(ctx context.Context, botID int64, text string) (botReply, error) {
	placeholder := strings.TrimSpace(text)
	if placeholder == "/empty" {
		placeholder = ""
	}
	if _, err := s.SetInlinePlaceholder(ctx, botID, placeholder); err != nil {
		return botReply{Text: fmt.Sprintf("Inline 占位提示文字最多只能有 %d 个字符，请重试或发送 /cancel。", domain.MaxBotInlinePlaceholderLen)}, err
	}
	if placeholder == "" {
		return botReply{Text: "成功！Inline 模式已关闭。请发送 /help 查看其他操作。"}, nil
	}
	return botReply{Text: "成功！Inline 设置已更新。请发送 /help 查看其他操作。"}, nil
}

func (s *Service) applySetInlineGeo(ctx context.Context, botID int64, text string) (botReply, error) {
	var enabled bool
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "enable", "on", "yes":
		enabled = true
	case "disable", "off", "no":
		enabled = false
	default:
		return botReply{Text: "请发送 'enable' 或 'disable'，或发送 /cancel。"}, domain.ErrBotInfoInvalid
	}
	if _, err := s.SetInlineGeo(ctx, botID, enabled); err != nil {
		s.log.Error("botfather: set inline geo", zap.Int64("bot_user_id", botID), zap.Error(err))
		return botReply{}, err
	}
	stateText := "关闭"
	if enabled {
		stateText = "启用"
	}
	return botReply{Text: fmt.Sprintf("成功！Inline 位置请求现已%s。", stateText)}, nil
}

func telegramLoginConfigurationPrompt(username string) string {
	return fmt.Sprintf(`请配置 @%s 的 Telegram Login。请逐条发送命令，或一次粘贴最多 %d 条、每行一条的命令：

add origin https://example.com
add redirect https://example.com/auth/callback
add ios com.example.app ABCDE12345 exampleapp://tglogin Example iOS App
add android com.example.app AA:BB:...:FF exampleapp://telegram-login Example Android App
remove origin https://example.com
remove redirect https://example.com/auth/callback
remove app 12
algorithm RS256|ES256|EdDSA|ES256K
enable
disable


Origin 用于授权 JS SDK 和旧版 login_url 按钮。Redirect 是精确匹配的 OIDC 回调地址。更改会立即生效。发送 /done 完成配置，或发送 /cancel 关闭会话；已经应用的更改不会撤销。`, username, maxTelegramLoginCommandsPerMessage)
}

func telegramLoginConfigurationContinuePrompt(username string) string {
	return fmt.Sprintf("仍在配置 @%s。请发送另一条命令，粘贴多条逐行命令，或发送 /done 完成配置。", username)
}

func (s *Service) finishTelegramLoginConfiguration(ctx context.Context, userID int64) botReply {
	state, found, err := s.bots.GetBotChatState(ctx, domain.BotFatherUserID, userID)
	if err != nil {
		s.log.Error("botfather: get telegram login state", zap.Int64("user_id", userID), zap.Error(err))
		return internalReply()
	}
	if !found || state.Command != botFatherCmdSetLogin || state.Step != botFatherStepValue {
		return botReply{Text: "当前没有正在进行的 Telegram Login 配置，请发送 /setlogin 开始配置。"}
	}
	botID, _ := strconv.ParseInt(state.Draft[botFatherDraftBotID], 10, 64)
	username := state.Draft[botFatherDraftBotUsername]
	if botID == 0 || username == "" {
		s.clearState(ctx, userID)
		return botReply{Text: "出了点问题，我忘记了正在编辑哪个机器人。请发送 /setlogin 重新开始。"}
	}
	owns, err := s.OwnsBot(ctx, userID, botID)
	if err != nil {
		s.log.Error("botfather: verify telegram login owner", zap.Int64("user_id", userID), zap.Int64("bot_user_id", botID), zap.Error(err))
		return internalReply()
	}
	if !owns {
		s.clearState(ctx, userID)
		return botReply{Text: "该机器人已不可用。"}
	}
	if s.telegramLogin == nil {
		s.clearState(ctx, userID)
		return botReply{Text: "此服务器未启用 Telegram Login。"}
	}
	configuration, configured, err := s.telegramLogin.ClientConfiguration(ctx, botID)
	if err != nil {
		s.log.Error("botfather: get telegram login configuration", zap.Int64("bot_user_id", botID), zap.Error(err))
		return internalReply()
	}
	if !configured {
		s.clearState(ctx, userID)
		return botReply{Text: fmt.Sprintf("@%s 尚未配置 Telegram Login，请发送 /setlogin 创建配置。", username)}
	}
	if err := s.bots.DeleteBotChatState(ctx, domain.BotFatherUserID, userID); err != nil {
		s.log.Error("botfather: finish telegram login state", zap.Int64("user_id", userID), zap.Error(err))
		return internalReply()
	}
	return botReply{Text: fmt.Sprintf("@%s 的 Telegram Login 配置已完成。\n\n%s", username, formatTelegramLoginConfiguration(username, configuration))}
}

func (s *Service) handleTelegramLoginConfigurationInput(
	ctx context.Context,
	state domain.BotChatState,
	botID int64,
	username string,
	text string,
) botReply {
	if strings.EqualFold(strings.TrimSpace(text), "done") {
		return s.finishTelegramLoginConfiguration(ctx, state.UserID)
	}
	lines := make([]string, 0, 4)
	for _, raw := range strings.Split(text, "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return botReply{Text: "请发送一条 Telegram Login 配置命令。\n\n" + telegramLoginConfigurationContinuePrompt(username)}
	}
	if len(lines) > maxTelegramLoginCommandsPerMessage {
		return botReply{Text: fmt.Sprintf("一条消息中的命令太多了，每次最多发送 %d 行。\n\n%s", maxTelegramLoginCommandsPerMessage, telegramLoginConfigurationContinuePrompt(username))}
	}

	applied := make([]string, 0, len(lines))
	for i, line := range lines {
		reply, err := s.applyTelegramLoginConfiguration(ctx, botID, username, line)
		if err != nil {
			if len(lines) == 1 {
				if reply.Text == "" {
					return internalReply()
				}
				return botReply{Text: reply.Text + "\n\n" + telegramLoginConfigurationContinuePrompt(username)}
			}
			failure := reply.Text
			if failure == "" {
				failure = "出了点问题，请稍后重试这一行。"
			}
			var out strings.Builder
			if len(applied) > 0 {
				fmt.Fprintf(&out, "发生错误前已应用 %d 条命令：\n%s\n\n", len(applied), strings.Join(applied, "\n"))
			}
			fmt.Fprintf(&out, "已在第 %d 行停止：\n%s\n\n", i+1, failure)
			if i+1 < len(lines) {
				fmt.Fprintf(&out, "后续 %d 条命令未应用。\n\n", len(lines)-i-1)
			}
			out.WriteString(telegramLoginConfigurationContinuePrompt(username))
			return botReply{Text: out.String()}
		}
		applied = append(applied, fmt.Sprintf("Line %d: %s", i+1, reply.Text))
	}

	var out strings.Builder
	if len(lines) == 1 {
		out.WriteString(strings.TrimPrefix(applied[0], "Line 1: "))
	} else {
		fmt.Fprintf(&out, "已应用全部 %d 条命令：\n%s", len(applied), strings.Join(applied, "\n"))
	}
	out.WriteString("\n\n")
	out.WriteString(telegramLoginConfigurationContinuePrompt(username))
	return botReply{Text: out.String()}
}

func formatTelegramLoginConfiguration(username string, configuration telegramloginapp.ClientConfiguration) string {
	status := "disabled"
	if configuration.Client.Enabled {
		status = "enabled"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "@%s 的 Telegram Login\n客户端 ID：%s\n状态：%s\n签名算法：%s\n密钥版本：%d",
		username, configuration.Client.ClientID, status, configuration.Client.SigningAlgorithm, configuration.Client.SecretVersion)
	if len(configuration.AllowedURLs) == 0 {
		out.WriteString("\n允许的 URL：无")
	} else {
		out.WriteString("\n允许的 URL：")
		for _, allowed := range configuration.AllowedURLs {
			fmt.Fprintf(&out, "\n- %s：%s", allowed.Kind, allowed.NormalizedURL)
		}
	}
	if len(configuration.NativeApps) > 0 {
		out.WriteString("\n原生应用：")
		for _, app := range configuration.NativeApps {
			fmt.Fprintf(&out, "\n- #%d %s %s [%s] -> %s（%s）", app.ID, app.Platform, app.ApplicationID, app.VerificationID, app.CallbackURI, app.VerifiedDisplayName)
		}
	}
	return out.String()
}

func telegramLoginAllowedURLKind(raw string) (domain.TelegramLoginAllowedURLKind, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "origin":
		return domain.TelegramLoginAllowedWebOrigin, true
	case "redirect":
		return domain.TelegramLoginAllowedRedirectURI, true
	default:
		return "", false
	}
}

func telegramLoginSigningAlgorithm(raw string) (domain.TelegramLoginSigningAlgorithm, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "RS256":
		return domain.TelegramLoginSigningRS256, true
	case "ES256":
		return domain.TelegramLoginSigningES256, true
	case "EDDSA":
		return domain.TelegramLoginSigningEdDSA, true
	case "ES256K":
		return domain.TelegramLoginSigningES256K, true
	default:
		return "", false
	}
}

func (s *Service) applyTelegramLoginConfiguration(ctx context.Context, botID int64, username, text string) (botReply, error) {
	if s.telegramLogin == nil {
		return botReply{Text: "此服务器未启用 Telegram Login。"}, domain.ErrTelegramLoginClientDisabled
	}
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 1 {
		switch strings.ToLower(fields[0]) {
		case "enable":
			if err := s.telegramLogin.SetClientEnabled(ctx, botID, true); err != nil {
				return botReply{}, err
			}
			return botReply{Text: fmt.Sprintf("@%s 的 Telegram Login 已启用。", username)}, nil
		case "disable":
			if err := s.telegramLogin.SetClientEnabled(ctx, botID, false); err != nil {
				return botReply{}, err
			}
			return botReply{Text: fmt.Sprintf("@%s 的 Telegram Login 已关闭，待处理请求将无法再获批或交换。", username)}, nil
		}
	}
	if len(fields) == 2 && strings.EqualFold(fields[0], "algorithm") {
		algorithm, ok := telegramLoginSigningAlgorithm(fields[1])
		if !ok {
			return botReply{Text: "未知的签名算法，请使用 RS256、ES256、EdDSA 或 ES256K，或发送 /cancel。"}, domain.ErrTelegramLoginClientInvalid
		}
		if _, err := s.telegramLogin.SetClientSigningAlgorithm(ctx, botID, algorithm); err != nil {
			if errors.Is(err, domain.ErrTelegramLoginClientInvalid) {
				return botReply{Text: fmt.Sprintf("此服务器没有为 %s 配置有效的签名密钥，因此无法使用该算法。请选择其他算法，或联系管理员轮换密钥环。", algorithm)}, err
			}
			return botReply{}, err
		}
		return botReply{Text: fmt.Sprintf("成功！@%s 的新 ID Token 将使用 %s。EdDSA 和 ES256K 仅接受 openid scope。", username, algorithm)}, nil
	}
	if len(fields) == 3 && (strings.EqualFold(fields[0], "add") || strings.EqualFold(fields[0], "remove")) &&
		(strings.EqualFold(fields[1], "origin") || strings.EqualFold(fields[1], "redirect")) {
		kind, ok := telegramLoginAllowedURLKind(fields[1])
		if !ok {
			return botReply{Text: "URL 类型必须是 origin 或 redirect，请重试或发送 /cancel。"}, domain.ErrTelegramLoginURLInvalid
		}
		if strings.EqualFold(fields[0], "add") {
			allowed, err := s.telegramLogin.AddAllowedURL(ctx, botID, kind, fields[2])
			if err != nil {
				return botReply{Text: "该 URL 不被允许。请使用此服务器允许的精确 HTTP(S) URL，且不能包含凭据、片段或 OAuth 保留查询字段。"}, err
			}
			return botReply{Text: fmt.Sprintf("成功！已为 @%s 添加 %s：\n%s", username, allowed.Kind, allowed.NormalizedURL)}, nil
		}
		deleted, err := s.telegramLogin.DeleteAllowedURL(ctx, botID, kind, fields[2])
		if err != nil {
			return botReply{Text: "该 URL 无效，请重试或发送 /cancel。"}, err
		}
		if !deleted {
			return botReply{Text: "未注册该精确 URL，请查看 /logininfo 后重试。"}, domain.ErrTelegramLoginURLInvalid
		}
		return botReply{Text: fmt.Sprintf("成功！已从 @%s 移除 %s。", username, kind)}, nil
	}
	if len(fields) >= 6 && strings.EqualFold(fields[0], "add") && (strings.EqualFold(fields[1], "ios") || strings.EqualFold(fields[1], "android")) {
		platform := domain.TelegramLoginNativeIOS
		if strings.EqualFold(fields[1], "android") {
			platform = domain.TelegramLoginNativeAndroid
		}
		app, err := s.telegramLogin.AddNativeApp(ctx, botID, platform, fields[2], fields[3], fields[4], strings.Join(fields[5:], " "))
		if err != nil {
			return botReply{Text: "原生应用注册无效。iOS 需要 Bundle ID 和 10 位 Team ID；Android 需要包名和 SHA-256 签名指纹。请使用精确的 HTTPS 回调地址或 custom scheme://host 回调地址。"}, err
		}
		return botReply{Text: fmt.Sprintf("成功！已为 @%s 注册原生应用 #%d：\n%s %s -> %s", username, app.ID, app.Platform, app.ApplicationID, app.CallbackURI)}, nil
	}
	if len(fields) == 3 && strings.EqualFold(fields[0], "remove") && strings.EqualFold(fields[1], "app") {
		appID, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || appID <= 0 {
			return botReply{Text: "原生应用 ID 必须是 /logininfo 中显示的正数。"}, domain.ErrTelegramLoginClientInvalid
		}
		deleted, err := s.telegramLogin.DeleteNativeApp(ctx, botID, appID)
		if err != nil {
			return botReply{}, err
		}
		if !deleted {
			return botReply{Text: "该机器人没有注册此原生应用，请查看 /logininfo。"}, domain.ErrTelegramLoginClientInvalid
		}
		return botReply{Text: fmt.Sprintf("成功！已从 @%s 移除原生应用 #%d。", username, appID)}, nil
	}
	return botReply{Text: telegramLoginConfigurationPrompt(username)}, domain.ErrTelegramLoginRequestInvalid
}

// applyToggle 解析 enable/disable 并设置 joingroups（join=true）或 privacy（join=false）。
func (s *Service) applyToggle(ctx context.Context, botID int64, text string, join bool) (botReply, error) {
	var on bool
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "enable", "on", "yes":
		on = true
	case "disable", "off", "no":
		on = false
	default:
		return botReply{Text: "请发送 'enable' 或 'disable'，或发送 /cancel。"}, domain.ErrBotInfoInvalid
	}
	var err error
	if join {
		_, err = s.SetJoinGroups(ctx, botID, on)
	} else {
		_, err = s.SetPrivacy(ctx, botID, on)
	}
	if err != nil {
		s.log.Error("botfather: apply toggle", zap.Int64("bot_user_id", botID), zap.Bool("join", join), zap.Error(err))
		return botReply{}, err
	}
	what := "群组隐私模式"
	if join {
		what = "群组加入权限"
	}
	state := "关闭"
	if on {
		state = "启用"
	}
	return botReply{Text: fmt.Sprintf("成功！%s现已%s。", what, state)}, nil
}

// okReply 把 service 调用结果转成提示：err==nil 回成功，否则回校验失败提示。
func okReply(err error, ok, fail string) botReply {
	if err != nil {
		return botReply{Text: fail}
	}
	return botReply{Text: ok}
}

// clearState 删除 BotFather 对话状态（忽略错误，仅记日志）。
func (s *Service) clearState(ctx context.Context, userID int64) {
	if err := s.bots.DeleteBotChatState(ctx, domain.BotFatherUserID, userID); err != nil {
		s.log.Error("botfather: delete chat state", zap.Int64("user_id", userID), zap.Error(err))
	}
}

type ownedBot struct {
	profile domain.BotProfile
	user    domain.User
}

func (s *Service) ownedBots(ctx context.Context, ownerUserID int64) ([]ownedBot, error) {
	profiles, err := s.bots.ListBotsByOwner(ctx, ownerUserID)
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(profiles))
	for _, p := range profiles {
		ids = append(ids, p.BotUserID)
	}
	users, err := s.users.ByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]domain.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	out := make([]ownedBot, 0, len(profiles))
	for _, p := range profiles {
		u, ok := byID[p.BotUserID]
		if !ok {
			continue
		}
		out = append(out, ownedBot{profile: p, user: u})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].user.ID < out[j].user.ID })
	return out, nil
}

func (s *Service) ownedBotUsernames(ctx context.Context, ownerUserID int64) ([]string, error) {
	owned, err := s.ownedBots(ctx, ownerUserID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(owned))
	for _, b := range owned {
		if b.user.Username != "" {
			out = append(out, b.user.Username)
		}
	}
	return out, nil
}

// tokenReply 拼装带 code entity 的 token 消息（全 ASCII 文本，offset 按字节即按
// UTF-16 code unit 成立）。
func tokenReply(head, token, tail string) botReply {
	return botReply{
		Text: head + token + tail,
		Entities: []domain.MessageEntity{
			{Type: domain.MessageEntityCode, Offset: len(head), Length: len(token)},
		},
	}
}

func internalReply() botReply {
	return botReply{Text: "出了点问题，请稍后重试。"}
}

// parseBotCommand 解析行首 "/cmd"（容忍 "/cmd@BotFather" 与尾随参数），返回小写命令名。
func parseBotCommand(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	cmd := text[1:]
	if i := strings.IndexAny(cmd, " \t\n"); i >= 0 {
		cmd = cmd[:i]
	}
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	if cmd == "" {
		return "", false
	}
	return strings.ToLower(cmd), true
}
