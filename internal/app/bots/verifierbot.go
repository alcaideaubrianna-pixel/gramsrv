package bots

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"telesrv/internal/branding"
	"telesrv/internal/domain"
)

// Built-in @verifierbot: the applicant front door for THIRD-PARTY verification
// (core.telegram.org/api/bots/verification).
//
// The difference from @verifybot is the whole reason this file exists, and it is
// the first thing every message here has to get across. The official checkmark is
// granted by the PLATFORM: an operator reviews the application @verifybot collected
// and flips the peer's verified flag. A third-party mark is granted by a VERIFIER
// BOT: the peer gets that bot's own icon before its name and that bot's line of
// description in its profile. Neither mechanism reads the other's state, and
// neither implies the other.
//
// This bot is the first verifier of a deployment -- the demo and the reference for
// the feature -- and its own verifier status (icon, company name, description
// permission) is granted by an operator in the admin panel, exactly like any other
// verifier's. That is the honesty constraint the dialog is built around: with no
// live, enabled BotVerifierSettings row this bot has no icon to put anywhere, so
// /start says so plainly and offers no apply button, instead of collecting
// paperwork nobody can act on.
//
// The dialog is button driven (inline keyboards) for every closed choice -- which
// peer, confirm/cancel, which mark to remove -- and text driven for the single
// free-form field (the justification). It owns no verification rule: verifier
// status, the per-verifier limit, the application status machine and the mark
// itself all live in app/botverification, and this file only renders the answers
// that service gives.
//
// Conversation state is domain.BotChatState (Command="verifier"), read and written
// under the per-user serviceBotReplyLock, so the Get->modify->Upsert round trip is
// atomic and replies stay ordered.

// customVerifications is the applicant-side surface of third-party verification
// used by this bot: app/botverification.Service satisfies it.
//
// It is declared as a narrow port rather than taken as a concrete service for the
// usual reason plus one specific to this feature: granting a mark is granting a
// badge, and the bot must not be able to reach past the service that owns the
// rules. Nothing here can write a mark, grant verifier status, decide an
// application or read another applicant's queue -- the operator's decision path is
// not on this interface at all.
//
// Once app/botverification lands, assert the contract in this package the way
// verifybot.go does for app/verification, so a signature change breaks the build
// instead of silently disabling the bot:
//
//	var _ customVerifications = (*botverificationapp.Service)(nil)
type customVerifications interface {
	// VerifierSettings reads this bot's own verifier status, or
	// domain.ErrVerifierNotFound when an operator has not granted it.
	VerifierSettings(ctx context.Context, botID int64) (domain.BotVerifierSettings, error)
	// CreateRequest files an application. A pending application on the same
	// (verifier, peer) pair reports domain.ErrCustomVerificationRequestExists.
	CreateRequest(ctx context.Context, req domain.CustomVerificationRequest) (domain.CustomVerificationRequest, error)
	// ApplicantRequests returns one applicant's own history, newest first.
	ApplicantRequests(ctx context.Context, applicantUserID int64, limit int) ([]domain.CustomVerificationRequest, error)
	// PendingRequest returns the live application for a (verifier, peer) pair, or
	// domain.ErrCustomVerificationRequestNotFound.
	PendingRequest(ctx context.Context, verifierBotID int64, peer domain.Peer) (domain.CustomVerificationRequest, error)
	// RevokeMark removes this verifier's mark from a peer. removed=false means there
	// was nothing to remove, which is what makes a repeated revoke a no-op.
	RevokeMark(ctx context.Context, verifierBotID int64, peer domain.Peer) (bool, error)
	// Marks lists granted marks. The bot only ever queries its own
	// (VerifierBotID, peer) pairs, so it can tell an applicant whether a mark is
	// actually live.
	Marks(ctx context.Context, filter domain.CustomVerificationFilter) ([]domain.CustomVerification, error)
}

// verifierBotTargets enumerates the peers an applicant may put forward: the bots
// they own, the channels and supergroups they created or administer, and their own
// account. app/verification.Service satisfies it as-is (EligibleTargets), which is
// why verificationApplications is accepted as a fallback source.
//
// Only the identity fields (Type, ID, Title, Username) are used here.
// VerificationTarget.Eligible and .Reason are deliberately IGNORED: they answer
// "may this peer be filed for the platform checkmark", which is a different bar
// belonging to a different mechanism -- a peer that is already officially verified,
// or has no public username, is a perfectly good third-party target. What the
// enumeration is trusted for is authorisation: it only ever contains peers the
// applicant owns or administers, which is why every button press re-resolves
// against it instead of trusting callback data.
type verifierBotTargets interface {
	EligibleTargets(ctx context.Context, applicantUserID int64) ([]domain.VerificationTarget, error)
}

// verifierBotApplicantNotifier mirrors app/botverification.ApplicantNotifier: the
// port that carries an operator's decision back to the applicant as an ordinary
// @verifierbot message. It is restated here so this package compiles the assertion
// below against its own copy of the contract; replace it with the real interface
// once that package exists.
type verifierBotApplicantNotifier interface {
	SendVerificationDecision(ctx context.Context, recipientUserID int64, req domain.CustomVerificationRequest) error
}

var _ verifierBotApplicantNotifier = (*Service)(nil)

const (
	// verifierBotCommand is the single BotChatState.Command of this dialog. There are
	// two flows (apply, revoke) and the step distinguishes them.
	verifierBotCommand = "verifier"

	verifierBotCmdVerify = "verify"
	verifierBotCmdStatus = "status"
	verifierBotCmdRevoke = "revoke"

	verifierStepIntro  = "intro"
	verifierStepTarget = "target"
	verifierStepReason = "reason"
	// verifierStepConfirm is the last button-only step of the apply flow.
	verifierStepConfirm = "confirm"
	// verifierStepDone is a terminal state that is deliberately *kept*: it is what
	// makes a second press of Confirm replay the same answer instead of filing a
	// second application or reporting an expired button.
	verifierStepDone          = "done"
	verifierStepRevokePick    = "revoke_pick"
	verifierStepRevokeConfirm = "revoke_confirm"

	verifierDraftTarget         = "tgt"
	verifierDraftTargetTitle    = "tgt_title"
	verifierDraftTargetUsername = "tgt_username"
	verifierDraftReason         = "reason"
	verifierDraftRequestID      = "req"
	// verifierDraftCorrelation is a per-attempt idempotency key handed to the
	// service. It is minted once when the summary is rendered and reused by every
	// repeat press of Confirm, so a double tap cannot become two applications even if
	// the dialog state is lost between them.
	verifierDraftCorrelation = "corr"
	// verifierDraftRevoked records the peer whose mark this dialog already removed
	// (and the label it was reported under), so a repeat press of the removal button
	// replays the same confirmation instead of reporting a second removal.
	verifierDraftRevoked      = "rev_done"
	verifierDraftRevokedLabel = "rev_done_label"
	verifierDraftGeneration   = "gen"
	// verifierDraftOptionPrefix namespaces the button token table inside the draft
	// map, so a token can never collide with a payload field.
	verifierDraftOptionPrefix = "opt:"

	// verifierCallbackDataPrefix tags this bot's own callback data. It carries no
	// information beyond "this is a @verifierbot button".
	verifierCallbackDataPrefix = "vfb:"
	// verifierOptionTokenBytes is the entropy behind one button token.
	verifierOptionTokenBytes = 6
	// verifierOptionTokenMaxLen bounds the token part of callback data before the
	// table is even consulted.
	verifierOptionTokenMaxLen = 32
	// verifierTokenGenerations is how many keyboard renders' worth of tokens stay
	// resolvable: the current render plus the two before it. Enough that pressing the
	// same button twice still resolves, while the table stays bounded.
	verifierTokenGenerations = 3

	// verifierMaxTargetButtons bounds one picker: an account can administer far more
	// peers than fit in an inline keyboard, and the token table is per-user durable
	// state, so the picker is truncated rather than unbounded.
	verifierMaxTargetButtons = 12
	// verifierStatusListLimit bounds the /status listing and the /revoke picker.
	verifierStatusListLimit = 10
	// verifierMinReasonLength is the floor on the justification. An operator has to
	// act on it, and "please verify me" is not something anyone can check.
	verifierMinReasonLength = 20
)

// Opaque button choices as stored in the per-user token table. These strings never
// travel over the wire: only the random token that maps to them does.
const (
	verifierChoiceApply       = "apply"
	verifierChoiceConfirm     = "confirm"
	verifierChoiceAbort       = "abort"
	verifierChoiceRevokeApply = "revoke"
	// The two peer-bearing choices carry the peer inside the token table entry, not
	// in the callback data. A confirmation button therefore names the peer it was
	// rendered for, so pressing a stale one cannot act on whatever peer the dialog
	// happens to be pointing at now.
	verifierChoiceTargetPrefix       = "tgt:"
	verifierChoiceRevokePrefix       = "rev:"
	verifierChoiceRevokeCommitPrefix = "revoke_commit:"
)

// verifierBotWhatText is the part of /start that is true whether or not an
// operator has activated this bot, so it is said first and unconditionally.
func verifierBotWhatText() string {
	return `我负责授予第三方认证标记。

第三方认证标记是认证方自己的图标，会显示在机器人、频道或账号名称前，并在资料中显示一行说明。它只表示“该认证方为此对象背书”。

它不是 ` + branding.ProductName() + ` 官方认证标记。平台认证由平台本身授予（@verifybot 收集申请）；第三方认证标记由运营认证机器人的公司授予。两者分别存储、显示和撤销，互不代表对方。`
}

func verifierBotHelpText() string {
	return `我是第三方认证机器人。我会在你的机器人、频道或账号名称前显示认证图标，并在资料中添加说明。这不是 ` + branding.ProductName() + ` 官方认证标记。

/start - 了解第三方认证及授予方
/verify - 申请认证标记
/status - 查看申请和已有标记
/revoke - 移除你授予对象的标记
/cancel - 放弃当前申请
/help - 显示此帮助

我不负责最终决定：我只收集申请，由运营人员授予或拒绝标记，并在这里通知结果。`
}

const (
	verifierBotIdleText = `我只负责授予第三方认证标记。发送 /verify 申请，发送 /status 查看申请状态，发送 /revoke 移除标记，或发送 /help 查看支持的命令。`

	// verifierNotActivatedText is the honest answer when this bot has no live
	// verifier status. It never pretends an application is possible.
	verifierNotActivatedText = `我目前还不能接受申请。

我是此服务器的内置认证机器人，认证状态需要由运营人员手动授予：运营人员必须在管理面板的机器人认证设置中，为我指定认证图标目录中的图标和背书公司名称，并保持该配置启用。在配置完成前，我没有可用于显示的图标，提交申请也只会产生无法处理的材料。

运营人员通知我已启用后，请再次发送 /start。发送 /help 查看其他命令。`

	verifierUnavailableText = `第三方认证当前在此服务器不可用。`

	verifierNoTargetSourceText = `目前无法查询你的机器人和频道，因此没有可供选择的对象。请稍后重试。`

	verifierNoTargetsText = `找不到可添加标记的对象。我可以认证你拥有的机器人、由你创建或管理的频道或超级群组，也可以认证你自己的账号。请先准备好其中一个对象，然后发送 /verify。`

	verifierNoRequestsText = `你还没有向我提交申请，也没有我的认证标记。发送 /verify 提交申请。`

	verifierNothingToRevokeText = `你没有我的认证标记，因此没有可移除的标记。发送 /status 查看申请状态。`

	verifierNothingToCancelText = `当前没有可取消的操作，请发送 /verify 申请认证标记。`

	verifierExpiredButtonText = `按钮已失效。发送 /verify 申请，或发送 /status 查看申请状态。`

	verifierPickButtonsText = `请使用我上方消息中的按钮。`

	verifierGoneTargetText = `该对象已不再属于你或由你管理，因此已移除。请发送 /verify 重新开始。`

	verifierApplyButtonText        = `申请认证`
	verifierConfirmButtonText      = `✅ 确认`
	verifierCancelButtonText       = `❌ 取消`
	verifierRevokeMenuButtonText   = `移除认证标记`
	verifierRevokeButtonText       = `✅ 移除标记`
	verifierRevokeCancelButtonText = `❌ 保留标记`
)

// verifierBotGlobalCommands are the commands honoured in every step, so an
// applicant is never trapped mid-dialog. Same contract as
// botFatherGlobalCommands.
var verifierBotGlobalCommands = map[string]bool{
	"start": true, "help": true, "cancel": true,
	verifierBotCmdVerify: true, verifierBotCmdStatus: true, verifierBotCmdRevoke: true,
}

// verifierOption is one inline button before its token is minted.
type verifierOption struct {
	text   string
	choice string
	style  domain.MarkupButtonStyle
}

// ---------------------------------------------------------------------------
// Message entry point
// ---------------------------------------------------------------------------

// respondAsVerifier generates and writes one @verifierbot reply. It is called from
// OnPrivateMessage's goroutine, serialised per user by serviceBotReplyLock, on a
// context detached from the user's already-answered RPC.
func (s *Service) respondAsVerifier(userID int64, msg domain.Message) {
	mu := s.serviceBotReplyLock(domain.VerifierBotUserID, userID)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if allowed, retryAfter := s.allowVerifierDialog(ctx, userID); !allowed {
		s.sendServiceBotReply(ctx, domain.VerifierBotUserID, userID, botReply{
			Text: verifierFloodText(retryAfter),
		})
		return
	}
	reply := s.handleVerifier(ctx, userID, msg.Body)
	s.sendServiceBotReply(ctx, domain.VerifierBotUserID, userID, reply)
}

// allowVerifierDialog bounds dialog traffic per applicant, keyed by this bot so it
// has its own budget rather than sharing @verifybot's. It is deliberately separate
// from any application limit inside the verification service: that one bounds how
// many applications exist, this one bounds how hard the state machine can be
// driven, so a script cannot burn writes without ever applying. A missing limiter
// means no bound, which is the pre-Redis behaviour.
func (s *Service) allowVerifierDialog(ctx context.Context, userID int64) (bool, int) {
	if s == nil || s.dialogLimiter == nil || s.dialogRateLimit <= 0 || s.dialogRateWindow <= 0 {
		return true, 0
	}
	key := fmt.Sprintf("bot-dialog:%d:%d", domain.VerifierBotUserID, userID)
	allowed, retryAfter, err := s.dialogLimiter.Allow(ctx, key, s.dialogRateLimit, s.dialogRateWindow)
	if err != nil {
		// A limiter outage must not silence the bot: fail open, log once.
		s.log.Warn("verifierbot: dialog rate limit check failed",
			zap.Int64("user_id", userID), zap.Error(err))
		return true, 0
	}
	return allowed, retryAfter
}

func verifierFloodText(retryAfter int) string {
	if retryAfter > 0 {
		return fmt.Sprintf("请求太频繁，请等待 %d 秒后重试。", retryAfter)
	}
	return "请求太频繁，请稍后重试。"
}

func (s *Service) handleVerifier(ctx context.Context, userID int64, body string) botReply {
	text := strings.TrimSpace(body)
	state, found, err := s.bots.GetBotChatState(ctx, domain.VerifierBotUserID, userID)
	if err != nil {
		s.log.Error("verifierbot: get chat state", zap.Int64("user_id", userID), zap.Error(err))
		return internalReply()
	}
	if found && state.Command != verifierBotCommand {
		// Unreachable dirty state (an older dialog shape): drop it rather than trap the
		// applicant in a step no handler owns.
		s.deleteVerifierState(ctx, userID)
		state, found = domain.BotChatState{}, false
	}
	if cmd, ok := parseBotCommand(text); ok {
		if verifierBotGlobalCommands[cmd] {
			return s.handleVerifierCommand(ctx, userID, cmd, state, found)
		}
		return botReply{Text: "无法识别此命令，请发送 /help 查看命令列表。"}
	}
	if !found {
		if text == "" {
			// Stickers and captionless media with no active dialog: stay silent instead of
			// answering every non-text message.
			return botReply{}
		}
		return botReply{Text: verifierBotIdleText}
	}
	if text == "" {
		return s.verifierStepReminder(state)
	}
	switch state.Step {
	case verifierStepReason:
		return s.handleVerifierReason(ctx, state, text)
	case verifierStepIntro, verifierStepDone:
		return botReply{Text: verifierBotIdleText}
	case verifierStepTarget, verifierStepConfirm, verifierStepRevokePick, verifierStepRevokeConfirm:
		// Button-only steps. Answered deliberately without re-rendering the keyboard: a
		// fresh render mints a new token generation and would eventually expire the very
		// buttons the applicant is looking at.
		return botReply{Text: verifierPickButtonsText}
	default:
		s.deleteVerifierState(ctx, userID)
		return botReply{Text: "出了点问题，我忘记了当前操作。请发送 /verify 重新开始。"}
	}
}

func (s *Service) handleVerifierCommand(ctx context.Context, userID int64, cmd string, state domain.BotChatState, found bool) botReply {
	if cmd == "help" {
		return botReply{Text: verifierBotHelpText()}
	}
	if s.customVerification == nil {
		return botReply{Text: verifierUnavailableText}
	}
	switch cmd {
	case "start":
		return s.verifierIntro(ctx, userID, state, found)
	case verifierBotCmdVerify:
		if !found {
			state = verifierNewState(userID)
		}
		return s.startVerifierRequest(ctx, state)
	case verifierBotCmdStatus:
		return s.verifierStatusReply(ctx, userID)
	case verifierBotCmdRevoke:
		if !found {
			state = verifierNewState(userID)
		}
		return s.verifierRevokePrompt(ctx, state, "")
	case "cancel":
		return s.cancelVerifierDialog(ctx, userID, state, found)
	default:
		return botReply{Text: verifierBotHelpText()}
	}
}

// ---------------------------------------------------------------------------
// Callback entry point
// ---------------------------------------------------------------------------

// onVerifierCallback answers one inline-button click on a @verifierbot message. It
// is reached from OnCallbackQuery (verifybot.go), which is the bots side of
// rpc.ServiceBotCallbacks.
func (s *Service) onVerifierCallback(ctx context.Context, query domain.BotCallbackQuery) (domain.BotCallbackAnswer, bool, error) {
	userID := query.UserID
	mu := s.serviceBotReplyLock(domain.VerifierBotUserID, userID)
	mu.Lock()
	defer mu.Unlock()

	if allowed, retryAfter := s.allowVerifierDialog(ctx, userID); !allowed {
		// Answering with an alert keeps the click from hanging for the whole callback
		// timeout, and the dialog state is left exactly as it was.
		return domain.BotCallbackAnswer{Message: verifierFloodText(retryAfter), Alert: true}, true, nil
	}

	state, found, err := s.bots.GetBotChatState(ctx, domain.VerifierBotUserID, userID)
	if err != nil {
		s.log.Error("verifierbot: get chat state for callback", zap.Int64("user_id", userID), zap.Error(err))
		return domain.BotCallbackAnswer{}, true, err
	}
	if !found || state.Command != verifierBotCommand {
		return verifierAlert(verifierExpiredButtonText), true, nil
	}
	choice, ok := verifierResolveOption(state, query.Data)
	if !ok {
		// The only way to reach this is callback data that is not in *this* user's own
		// token table: a button from an expired generation, or data that was replayed or
		// fabricated. All of them are refused identically and change nothing at all.
		return verifierAlert(verifierExpiredButtonText), true, nil
	}
	if s.customVerification == nil {
		return verifierAlert(verifierUnavailableText), true, nil
	}
	// The follow-up message is written on a context detached from the caller's RPC:
	// the answer unblocks the click, and the message must survive the client hanging
	// up immediately afterwards.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	reply, answer := s.applyVerifierChoice(sendCtx, state, choice)
	s.sendServiceBotReply(sendCtx, domain.VerifierBotUserID, userID, reply)
	return answer, true, nil
}

// applyVerifierChoice executes one resolved button choice.
func (s *Service) applyVerifierChoice(ctx context.Context, state domain.BotChatState, choice string) (botReply, domain.BotCallbackAnswer) {
	switch {
	case choice == verifierChoiceApply:
		return s.startVerifierRequest(ctx, state), domain.BotCallbackAnswer{}
	case choice == verifierChoiceAbort:
		return s.cancelVerifierDialog(ctx, state.UserID, state, true), domain.BotCallbackAnswer{}
	case choice == verifierChoiceConfirm:
		return s.submitVerifierRequest(ctx, state), domain.BotCallbackAnswer{}
	case choice == verifierChoiceRevokeApply:
		return s.verifierRevokePrompt(ctx, state, ""), domain.BotCallbackAnswer{}
	case strings.HasPrefix(choice, verifierChoiceRevokeCommitPrefix):
		return s.commitVerifierRevoke(ctx, state, strings.TrimPrefix(choice, verifierChoiceRevokeCommitPrefix)), domain.BotCallbackAnswer{}
	case strings.HasPrefix(choice, verifierChoiceTargetPrefix):
		return s.chooseVerifierTarget(ctx, state, strings.TrimPrefix(choice, verifierChoiceTargetPrefix)), domain.BotCallbackAnswer{}
	case strings.HasPrefix(choice, verifierChoiceRevokePrefix):
		return s.chooseVerifierRevokeTarget(ctx, state, strings.TrimPrefix(choice, verifierChoiceRevokePrefix)), domain.BotCallbackAnswer{}
	default:
		s.log.Warn("verifierbot: unknown button choice",
			zap.Int64("user_id", state.UserID), zap.String("choice", choice))
		return botReply{}, verifierAlert(verifierExpiredButtonText)
	}
}

// verifierAlert is a refusal shown as a modal on the clicking client. A refusal is
// deliberately an alert rather than a toast: it is the only feedback the applicant
// gets, because a refused click writes no message.
func verifierAlert(text string) domain.BotCallbackAnswer {
	return domain.BotCallbackAnswer{Alert: true, Message: verifierTruncate(text, domain.MaxBotCallbackAnswerLen)}
}

// ---------------------------------------------------------------------------
// Verifier status
// ---------------------------------------------------------------------------

// verifierSettings resolves this bot's own verifier status. The bool is the whole
// point of the function: false means an operator has not activated the bot, and the
// returned reply says exactly that instead of hiding it behind a generic failure.
func (s *Service) verifierSettings(ctx context.Context, userID int64) (domain.BotVerifierSettings, botReply, bool) {
	if s.customVerification == nil {
		return domain.BotVerifierSettings{}, botReply{Text: verifierUnavailableText}, false
	}
	settings, err := s.customVerification.VerifierSettings(ctx, domain.VerifierBotUserID)
	switch {
	case errors.Is(err, domain.ErrVerifierNotFound), errors.Is(err, domain.ErrVerifierForbidden):
		return domain.BotVerifierSettings{}, botReply{Text: verifierNotActivatedText}, false
	case err != nil:
		return domain.BotVerifierSettings{}, s.verifierErrorReply(userID, "read verifier settings", err), false
	}
	if !settings.Enabled {
		// The operator's kill switch. Existing marks stay, but nothing new can be
		// granted, so promising an application would be a lie.
		return domain.BotVerifierSettings{}, botReply{Text: verifierNotActivatedText}, false
	}
	if err := settings.Validate(); err != nil {
		// A half-written verifier row (no icon, no company) cannot mark anything either.
		// Report it as not activated, and log it: this one is an operator mistake.
		s.log.Warn("verifierbot: verifier settings do not validate", zap.Error(err))
		return domain.BotVerifierSettings{}, botReply{Text: verifierNotActivatedText}, false
	}
	return settings, botReply{}, true
}

// verifierMarkDescription is the description a mark granted now would carry. It is
// resolved through the domain rule rather than read off the settings struct, so
// this dialog and the RPC edge can never disagree about it.
func verifierMarkDescription(settings domain.BotVerifierSettings) string {
	description, err := settings.DescriptionFor("")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(description)
}

func verifierDescriptionLine(settings domain.BotVerifierSettings) string {
	if description := verifierMarkDescription(settings); description != "" {
		return description
	}
	return "(no description -- only the icon is shown)"
}

// ---------------------------------------------------------------------------
// Flow: /start
// ---------------------------------------------------------------------------

// verifierIntro answers /start. The explanation of what a third-party mark is
// comes first and unconditionally; the apply button only exists when there is a
// live verifier status behind it.
func (s *Service) verifierIntro(ctx context.Context, userID int64, state domain.BotChatState, found bool) botReply {
	if !found {
		state = verifierNewState(userID)
	}
	settings, refusal, ok := s.verifierSettings(ctx, userID)
	if !ok {
		// No keyboard, no state write: there is nothing for the applicant to press.
		return botReply{Text: verifierJoin(verifierBotWhatText(), refusal.Text)}
	}
	state.Step = verifierStepIntro
	markup := s.verifierOptionKeyboard(&state, [][]verifierOption{{
		{text: verifierApplyButtonText, choice: verifierChoiceApply, style: domain.MarkupButtonStylePrimary},
	}})
	if !s.saveVerifierState(ctx, state) {
		return internalReply()
	}
	live := fmt.Sprintf("认证方：%s\n\n我会为你的对象添加以下标记：\n%s\n\n点击下方按钮或发送 /verify 申请。每份申请都会由运营人员审核决定，我只负责收集。发送 /help 查看其他命令。",
		verifierTruncate(strings.TrimSpace(settings.CompanyName), domain.MaxVerifierCompanyLength),
		verifierDescriptionLine(settings))
	return botReply{Text: verifierJoin(verifierBotWhatText(), live), ReplyMarkup: markup}
}

// ---------------------------------------------------------------------------
// Flow: apply
// ---------------------------------------------------------------------------

// startVerifierRequest is /verify and the apply button: it starts a fresh
// application, so the leftovers of any previous one are cleared first.
func (s *Service) startVerifierRequest(ctx context.Context, state domain.BotChatState) botReply {
	settings, refusal, ok := s.verifierSettings(ctx, state.UserID)
	if !ok {
		return refusal
	}
	verifierEnsureDraft(&state)
	for _, key := range []string{
		verifierDraftTarget, verifierDraftTargetTitle, verifierDraftTargetUsername,
		verifierDraftReason, verifierDraftRequestID, verifierDraftCorrelation,
		verifierDraftRevoked, verifierDraftRevokedLabel,
	} {
		delete(state.Draft, key)
	}
	return s.verifierTargetPrompt(ctx, state, settings, "")
}

// verifierTargetPrompt renders the peer picker.
func (s *Service) verifierTargetPrompt(ctx context.Context, state domain.BotChatState, settings domain.BotVerifierSettings, lead string) botReply {
	targets, reply, ok := s.verifierCandidates(ctx, state.UserID)
	if !ok {
		return botReply{Text: verifierJoin(lead, reply.Text)}
	}
	state.Step = verifierStepTarget
	rows := make([][]verifierOption, 0, len(targets)+1)
	for _, target := range targets {
		rows = append(rows, []verifierOption{{
			text:   verifierTargetButtonText(target),
			choice: verifierChoiceTargetPrefix + string(target.Type) + ":" + strconv.FormatInt(target.ID, 10),
		}})
	}
	rows = append(rows, []verifierOption{{
		text:   verifierCancelButtonText,
		choice: verifierChoiceAbort,
		style:  domain.MarkupButtonStyleDanger,
	}})
	markup := s.verifierOptionKeyboard(&state, rows)
	if !s.saveVerifierState(ctx, state) {
		return internalReply()
	}
	prompt := fmt.Sprintf("你想为哪个对象添加 %s 标记？请从下方选择。",
		verifierTruncate(strings.TrimSpace(settings.CompanyName), domain.MaxVerifierCompanyLength))
	return botReply{Text: verifierJoin(lead, prompt), ReplyMarkup: markup}
}

// verifierCandidates enumerates the applicant's own peers, bounded and filtered
// down to what a third-party mark can actually be attached to.
func (s *Service) verifierCandidates(ctx context.Context, userID int64) ([]domain.VerificationTarget, botReply, bool) {
	source := s.verifierTargetSource()
	if source == nil {
		return nil, botReply{Text: verifierNoTargetSourceText}, false
	}
	targets, err := source.EligibleTargets(ctx, userID)
	if err != nil {
		s.log.Warn("verifierbot: enumerate targets", zap.Int64("user_id", userID), zap.Error(err))
		return nil, botReply{Text: verifierNoTargetSourceText}, false
	}
	out := make([]domain.VerificationTarget, 0, len(targets))
	for _, target := range targets {
		if !verifierTargetVerifiable(target) {
			continue
		}
		out = append(out, target)
		if len(out) >= verifierMaxTargetButtons {
			break
		}
	}
	if len(out) == 0 {
		return nil, botReply{Text: verifierNoTargetsText}, false
	}
	return out, botReply{}, true
}

// verifierTargetVerifiable filters the enumeration down to peers a mark can exist
// on: the domain accepts user and channel peers only, and a built-in service
// account is never a third-party subject.
func verifierTargetVerifiable(target domain.VerificationTarget) bool {
	if target.ID <= 0 || !target.Type.Valid() {
		return false
	}
	peer := verifierTargetPeer(target)
	if peer.Type == domain.PeerTypeUser && domain.IsSystemUserID(peer.ID) {
		return false
	}
	return peer.Type == domain.PeerTypeUser || peer.Type == domain.PeerTypeChannel
}

func verifierTargetPeer(target domain.VerificationTarget) domain.Peer {
	return domain.Peer{Type: target.Type.PeerType(), ID: target.ID}
}

// chooseVerifierTarget opens the application for the picked peer.
//
// The kind and id come from the token table, i.e. from state this server wrote when
// it rendered the keyboard -- never from the callback data. They are then
// re-resolved against the applicant's own enumeration, which is what authorises the
// choice: a peer that is no longer theirs simply is not in the list any more.
func (s *Service) chooseVerifierTarget(ctx context.Context, state domain.BotChatState, payload string) botReply {
	kind, rawID, split := strings.Cut(payload, ":")
	targetID, err := strconv.ParseInt(rawID, 10, 64)
	targetType := domain.VerificationTargetType(kind)
	if !split || err != nil || targetID <= 0 || !targetType.Valid() {
		s.log.Warn("verifierbot: malformed target choice",
			zap.Int64("user_id", state.UserID), zap.String("payload", payload))
		return botReply{Text: verifierExpiredButtonText}
	}
	settings, refusal, ok := s.verifierSettings(ctx, state.UserID)
	if !ok {
		return refusal
	}
	targets, reply, ok := s.verifierCandidates(ctx, state.UserID)
	if !ok {
		return reply
	}
	var target domain.VerificationTarget
	for _, candidate := range targets {
		if candidate.Type == targetType && candidate.ID == targetID {
			target = candidate
		}
	}
	if target.ID == 0 {
		return botReply{Text: verifierGoneTargetText}
	}
	peer := verifierTargetPeer(target)
	label := verifierPeerLabel(target.Title, target.Username, peer)

	// Already applied for, or already marked: say so instead of filing a duplicate the
	// service would refuse anyway.
	if pending, found := s.verifierPendingRequest(ctx, peer); found {
		return botReply{Text: fmt.Sprintf("对象 %s 的申请 #%d 已交给运营人员处理，无需重复提交。发送 /status 查看状态。",
			label, pending.ID)}
	}
	if mark, found := s.verifierMark(ctx, peer); found {
		text := fmt.Sprintf("%s 已有我的认证标记，无需重复申请。", label)
		if description := strings.TrimSpace(mark.Description); description != "" {
			text += "\n\n说明：" + verifierTruncate(description, domain.MaxCustomVerificationDescriptionLength)
		}
		// The picker stays where it is (the dialog is still at the subject step): this
		// only offers the way out, which is removing the mark.
		markup := s.verifierOptionKeyboard(&state, [][]verifierOption{
			{{text: verifierRevokeMenuButtonText, choice: verifierChoiceRevokeApply, style: domain.MarkupButtonStyleDanger}},
		})
		if !s.saveVerifierState(ctx, state) {
			return internalReply()
		}
		return botReply{Text: text + "\n\n如需移除，请发送 /revoke 或点击下方按钮。", ReplyMarkup: markup}
	}

	verifierEnsureDraft(&state)
	state.Draft[verifierDraftTarget] = string(targetType) + ":" + strconv.FormatInt(targetID, 10)
	state.Draft[verifierDraftTargetTitle] = target.Title
	state.Draft[verifierDraftTargetUsername] = target.Username
	delete(state.Draft, verifierDraftCorrelation)
	return s.verifierAdvance(ctx, state, settings, verifierStepReason, "对象："+label+"。")
}

func (s *Service) handleVerifierReason(ctx context.Context, state domain.BotChatState, text string) botReply {
	length := utf8.RuneCountInString(text)
	if length < verifierMinReasonLength {
		return botReply{Text: fmt.Sprintf("当前有 %d 个字符，至少需要 %d 个。运营人员需要据此审核，请说明由谁运营以及对象因何知名。",
			length, verifierMinReasonLength)}
	}
	if length > domain.MaxCustomVerificationReasonLength {
		return botReply{Text: fmt.Sprintf("当前有 %d 个字符，限制为 %d 个，请缩短内容。",
			length, domain.MaxCustomVerificationReasonLength)}
	}
	settings, refusal, ok := s.verifierSettings(ctx, state.UserID)
	if !ok {
		return refusal
	}
	verifierEnsureDraft(&state)
	state.Draft[verifierDraftReason] = text
	return s.verifierAdvance(ctx, state, settings, verifierStepConfirm, "已保存。")
}

// submitVerifierRequest files the application.
//
// Pressing Confirm twice is idempotent by construction: the first press moves the
// dialog to verifierStepDone, and a repeat press is answered from the id stored in
// the dialog instead of calling CreateRequest again.
func (s *Service) submitVerifierRequest(ctx context.Context, state domain.BotChatState) botReply {
	if state.Step == verifierStepDone {
		if id := verifierDraftInt(state, verifierDraftRequestID); id > 0 {
			return botReply{Text: verifierFiledText(id, verifierDraftTargetLabel(state))}
		}
		return botReply{Text: verifierBotIdleText}
	}
	settings, refusal, ok := s.verifierSettings(ctx, state.UserID)
	if !ok {
		return refusal
	}
	target, targetOK := verifierDraftTargetOf(state)
	reason := strings.TrimSpace(state.Draft[verifierDraftReason])
	if !targetOK {
		return s.startVerifierRequest(ctx, state)
	}
	if utf8.RuneCountInString(reason) < verifierMinReasonLength {
		return s.verifierAdvance(ctx, state, settings, verifierStepReason, "仍需要填写申请理由。")
	}
	peer := verifierTargetPeer(target)
	label := verifierDraftTargetLabel(state)
	verifierEnsureDraft(&state)
	correlation := state.Draft[verifierDraftCorrelation]
	if correlation == "" {
		correlation = s.verifierCorrelationID(state.UserID)
		state.Draft[verifierDraftCorrelation] = correlation
	}
	created, err := s.customVerification.CreateRequest(ctx, domain.CustomVerificationRequest{
		VerifierBotID:   domain.VerifierBotUserID,
		ApplicantUserID: state.UserID,
		Peer:            peer,
		PeerTitle:       state.Draft[verifierDraftTargetTitle],
		PeerUsername:    state.Draft[verifierDraftTargetUsername],
		Reason:          reason,
		// RequestedDescription is deliberately empty: the mark carries the verifier's
		// own description, resolved by domain.BotVerifierSettings.DescriptionFor when the
		// operator grants it. Freezing today's text into the application would let the
		// granted mark drift from what the verifier actually says.
		Status:        domain.CustomVerificationPending,
		CorrelationID: correlation,
	})
	if errors.Is(err, domain.ErrCustomVerificationRequestExists) {
		// Someone (another device, or this dialog after a lost reply) already filed it.
		// Adopt that application instead of reporting a refusal for something the
		// applicant actually got.
		if pending, found := s.verifierPendingRequest(ctx, peer); found {
			state.Step = verifierStepDone
			state.Draft[verifierDraftRequestID] = strconv.FormatInt(pending.ID, 10)
			if !s.saveVerifierState(ctx, state) {
				return internalReply()
			}
			return botReply{Text: verifierFiledText(pending.ID, label)}
		}
	}
	if err != nil {
		return s.verifierErrorReply(state.UserID, "create request", err)
	}
	state.Step = verifierStepDone
	state.Draft[verifierDraftRequestID] = strconv.FormatInt(created.ID, 10)
	if !s.saveVerifierState(ctx, state) {
		return internalReply()
	}
	return botReply{Text: verifierFiledText(created.ID, label)}
}

// cancelVerifierDialog drops the dialog. It cannot withdraw an application that is
// already with the operator, and says so rather than implying otherwise.
func (s *Service) cancelVerifierDialog(ctx context.Context, userID int64, state domain.BotChatState, found bool) botReply {
	if !found {
		return botReply{Text: verifierNothingToCancelText}
	}
	filed := verifierDraftInt(state, verifierDraftRequestID)
	s.deleteVerifierState(ctx, userID)
	if filed > 0 && state.Step == verifierStepDone {
		return botReply{Text: fmt.Sprintf("已放弃当前对话，但申请 #%d 已交给运营人员处理，仍会保留在队列中。发送 /status 查看状态。", filed)}
	}
	return botReply{Text: "已放弃对话，未提交申请。需要申请时请发送 /verify。"}
}

// ---------------------------------------------------------------------------
// Flow: /status
// ---------------------------------------------------------------------------

func (s *Service) verifierStatusReply(ctx context.Context, userID int64) botReply {
	requests, err := s.customVerification.ApplicantRequests(ctx, userID, verifierStatusListLimit)
	if err != nil {
		return s.verifierErrorReply(userID, "list requests", err)
	}
	if len(requests) == 0 {
		return botReply{Text: verifierNoRequestsText}
	}
	var b strings.Builder
	b.WriteString("你的第三方认证申请：")
	live := 0
	for _, req := range requests {
		label := verifierPeerLabel(req.PeerTitle, req.PeerUsername, req.Peer)
		b.WriteString("\n\n#")
		b.WriteString(strconv.FormatInt(req.ID, 10))
		b.WriteString(" - ")
		b.WriteString(label)
		b.WriteString("\n状态：")
		b.WriteString(verifierStatusLabel(req.Status))
		if date := verifierDateLabel(req); date != "" {
			b.WriteString(" (")
			b.WriteString(date)
			b.WriteString(")")
		}
		// Only DecisionReason is ever shown: it is the text written to be read by the
		// applicant. req.InternalNote is the operator's private note and must never
		// appear in a bot message.
		if req.Status == domain.CustomVerificationRejected || req.Status == domain.CustomVerificationRevoked {
			if reason := strings.TrimSpace(req.DecisionReason); reason != "" {
				b.WriteString("\n原因：")
				b.WriteString(reason)
			}
		}
		if req.Status == domain.CustomVerificationApproved {
			// The application says "approved"; whether the mark is still on the peer is a
			// different fact, so it is read rather than inferred.
			if mark, found := s.verifierMark(ctx, req.Peer); found {
				live++
				b.WriteString("\n标记：有效")
				if description := strings.TrimSpace(mark.Description); description != "" {
					b.WriteString(" -- ")
					b.WriteString(verifierTruncate(description, 200))
				}
			} else {
				b.WriteString("\n标记：已不在该对象上")
			}
		}
	}
	if live > 0 {
		b.WriteString("\n\n发送 /revoke 移除认证标记。")
	} else {
		b.WriteString("\n\n发送 /verify 为其他对象申请认证。")
	}
	return botReply{Text: b.String()}
}

// ---------------------------------------------------------------------------
// Flow: /revoke
// ---------------------------------------------------------------------------

// verifierRevokePrompt renders the picker of marks this applicant can remove.
//
// The list is built from the applicant's OWN approved applications, intersected
// with the marks that are actually live. That is what authorises the removal: a
// peer nobody filed under this account can never appear here, so the picker cannot
// be used to strip a mark off somebody else's channel.
func (s *Service) verifierRevokePrompt(ctx context.Context, state domain.BotChatState, lead string) botReply {
	marked, reply, ok := s.verifierRevocable(ctx, state.UserID)
	if !ok {
		return botReply{Text: verifierJoin(lead, reply.Text)}
	}
	state.Step = verifierStepRevokePick
	verifierEnsureDraft(&state)
	rows := make([][]verifierOption, 0, len(marked)+1)
	for _, req := range marked {
		rows = append(rows, []verifierOption{{
			text:   verifierTruncate(verifierPeerLabel(req.PeerTitle, req.PeerUsername, req.Peer), 64),
			choice: verifierChoiceRevokePrefix + verifierPeerKey(req.Peer),
		}})
	}
	rows = append(rows, []verifierOption{{
		text:   verifierCancelButtonText,
		choice: verifierChoiceAbort,
		style:  domain.MarkupButtonStyleDanger,
	}})
	markup := s.verifierOptionKeyboard(&state, rows)
	if !s.saveVerifierState(ctx, state) {
		return internalReply()
	}
	return botReply{
		Text:        verifierJoin(lead, "要移除哪个认证标记？图标和资料说明会从资料页消失，如需恢复必须重新提交申请。"),
		ReplyMarkup: markup,
	}
}

// verifierRevocable returns the applicant's approved applications whose mark is
// still live, one per peer.
func (s *Service) verifierRevocable(ctx context.Context, userID int64) ([]domain.CustomVerificationRequest, botReply, bool) {
	requests, err := s.customVerification.ApplicantRequests(ctx, userID, verifierStatusListLimit)
	if err != nil {
		return nil, s.verifierErrorReply(userID, "list requests for revoke", err), false
	}
	seen := make(map[domain.Peer]struct{}, len(requests))
	out := make([]domain.CustomVerificationRequest, 0, len(requests))
	for _, req := range requests {
		if req.Status != domain.CustomVerificationApproved {
			continue
		}
		if _, duplicate := seen[req.Peer]; duplicate {
			continue
		}
		if _, found := s.verifierMark(ctx, req.Peer); !found {
			continue
		}
		seen[req.Peer] = struct{}{}
		out = append(out, req)
		if len(out) >= verifierMaxTargetButtons {
			break
		}
	}
	if len(out) == 0 {
		return nil, botReply{Text: verifierNothingToRevokeText}, false
	}
	return out, botReply{}, true
}

// chooseVerifierRevokeTarget asks for confirmation before anything is removed.
func (s *Service) chooseVerifierRevokeTarget(ctx context.Context, state domain.BotChatState, payload string) botReply {
	peer, ok := verifierParsePeer(payload)
	if !ok {
		s.log.Warn("verifierbot: malformed revoke choice",
			zap.Int64("user_id", state.UserID), zap.String("payload", payload))
		return botReply{Text: verifierExpiredButtonText}
	}
	marked, reply, ok := s.verifierRevocable(ctx, state.UserID)
	if !ok {
		return reply
	}
	var chosen domain.CustomVerificationRequest
	for _, req := range marked {
		if req.Peer == peer {
			chosen = req
		}
	}
	if chosen.ID == 0 {
		return botReply{Text: verifierNothingToRevokeText}
	}
	verifierEnsureDraft(&state)
	state.Step = verifierStepRevokeConfirm
	markup := s.verifierOptionKeyboard(&state, [][]verifierOption{
		{{
			text:   verifierRevokeButtonText,
			choice: verifierChoiceRevokeCommitPrefix + verifierPeerKey(peer),
			style:  domain.MarkupButtonStyleDanger,
		}},
		{{text: verifierRevokeCancelButtonText, choice: verifierChoiceAbort}},
	})
	if !s.saveVerifierState(ctx, state) {
		return internalReply()
	}
	return botReply{
		Text: fmt.Sprintf("要从 %s 移除我的认证标记吗？\n\n名称前的图标和资料中的说明会立即消失。申请 #%d 会在历史记录中保留为已撤销，如需重新获得标记，必须提交新申请。",
			verifierPeerLabel(chosen.PeerTitle, chosen.PeerUsername, peer), chosen.ID),
		ReplyMarkup: markup,
	}
}

// commitVerifierRevoke removes the mark from the peer the pressed button was
// rendered for.
//
// A repeat press replays the same answer: the removed peer is recorded in the
// dialog together with the label it was reported under. A press whose peer is not
// the recorded one is not a replay and goes through the authorisation check again,
// so a stale confirmation can only ever be refused -- never applied to a peer the
// applicant did not confirm.
func (s *Service) commitVerifierRevoke(ctx context.Context, state domain.BotChatState, payload string) botReply {
	if state.Step == verifierStepDone && payload != "" && state.Draft[verifierDraftRevoked] == payload {
		return botReply{Text: verifierRevokedText(state.Draft[verifierDraftRevokedLabel])}
	}
	peer, ok := verifierParsePeer(payload)
	if !ok {
		s.log.Warn("verifierbot: malformed revoke commit",
			zap.Int64("user_id", state.UserID), zap.String("payload", payload))
		return botReply{Text: verifierExpiredButtonText}
	}
	// Re-authorise at the moment of the write: the picker's list is the only thing
	// that says this peer is the applicant's, and it was rendered some time ago.
	marked, reply, ok := s.verifierRevocable(ctx, state.UserID)
	if !ok {
		return reply
	}
	var chosen domain.CustomVerificationRequest
	for _, req := range marked {
		if req.Peer == peer {
			chosen = req
		}
	}
	if chosen.ID == 0 {
		return botReply{Text: verifierNothingToRevokeText}
	}
	label := verifierPeerLabel(chosen.PeerTitle, chosen.PeerUsername, peer)
	// The mark carries no revocation audit column, so the actor and the reason are
	// logged here rather than pretended into the store's signature.
	s.log.Info("verifierbot: applicant removed own mark",
		zap.Int64("user_id", state.UserID),
		zap.String("peer_type", string(peer.Type)),
		zap.Int64("peer_id", peer.ID))
	removed, err := s.customVerification.RevokeMark(ctx, domain.VerifierBotUserID, peer)
	if err != nil {
		return s.verifierErrorReply(state.UserID, "revoke mark", err)
	}
	if !removed {
		// The mark went away between the picker and this press. Nothing is recorded as
		// removed by this dialog, so a repeat press re-checks rather than replaying a
		// removal that never happened here.
		return botReply{Text: fmt.Sprintf("%s 上已经没有认证标记，因此没有任何更改。", label)}
	}
	verifierEnsureDraft(&state)
	state.Step = verifierStepDone
	state.Draft[verifierDraftRevoked] = payload
	state.Draft[verifierDraftRevokedLabel] = label
	if !s.saveVerifierState(ctx, state) {
		return internalReply()
	}
	return botReply{Text: verifierRevokedText(label)}
}

func verifierRevokedText(label string) string {
	if strings.TrimSpace(label) == "" {
		label = "that peer"
	}
	return fmt.Sprintf("已从 %s 移除我的认证标记，资料中的图标和说明已消失。需要再次申请时请发送 /verify。", label)
}

// ---------------------------------------------------------------------------
// Step rendering
// ---------------------------------------------------------------------------

// verifierAdvance moves the dialog to step and renders that step's prompt. lead is
// an optional acknowledgement of what was just accepted.
func (s *Service) verifierAdvance(ctx context.Context, state domain.BotChatState, settings domain.BotVerifierSettings, step, lead string) botReply {
	state.Step = step
	verifierEnsureDraft(&state)
	var reply botReply
	switch step {
	case verifierStepTarget:
		return s.verifierTargetPrompt(ctx, state, settings, lead)
	case verifierStepReason:
		markup := s.verifierOptionKeyboard(&state, [][]verifierOption{{
			{text: verifierCancelButtonText, choice: verifierChoiceAbort, style: domain.MarkupButtonStyleDanger},
		}})
		reply = botReply{Text: verifierJoin(lead, verifierReasonPrompt(settings, verifierDraftTargetLabel(state))), ReplyMarkup: markup}
	case verifierStepConfirm:
		if state.Draft[verifierDraftCorrelation] == "" {
			state.Draft[verifierDraftCorrelation] = s.verifierCorrelationID(state.UserID)
		}
		markup := s.verifierOptionKeyboard(&state, [][]verifierOption{
			{{text: verifierConfirmButtonText, choice: verifierChoiceConfirm, style: domain.MarkupButtonStyleSuccess}},
			{{text: verifierCancelButtonText, choice: verifierChoiceAbort, style: domain.MarkupButtonStyleDanger}},
		})
		reply = botReply{Text: verifierJoin(lead, verifierSummaryText(state, settings)), ReplyMarkup: markup}
	default:
		s.log.Error("verifierbot: advance to unknown step",
			zap.Int64("user_id", state.UserID), zap.String("step", step))
		return internalReply()
	}
	if !s.saveVerifierState(ctx, state) {
		return internalReply()
	}
	return reply
}

func (s *Service) verifierStepReminder(state domain.BotChatState) botReply {
	switch state.Step {
	case verifierStepReason:
		return botReply{Text: fmt.Sprintf("请在一条消息中填写理由，字数需在 %d 到 %d 个字符之间。",
			verifierMinReasonLength, domain.MaxCustomVerificationReasonLength)}
	case verifierStepTarget, verifierStepConfirm, verifierStepRevokePick, verifierStepRevokeConfirm:
		return botReply{Text: verifierPickButtonsText}
	default:
		return botReply{Text: verifierBotIdleText}
	}
}

func verifierReasonPrompt(settings domain.BotVerifierSettings, label string) string {
	company := strings.TrimSpace(settings.CompanyName)
	if company == "" {
		company = "this verifier"
	}
	return fmt.Sprintf("请用一条消息说明 %s 为什么应该为 %s 背书：由谁运营、对象因何知名，以及运营人员可以核查的内容。字数需在 %d 到 %d 个字符之间。",
		company, label, verifierMinReasonLength, domain.MaxCustomVerificationReasonLength)
}

func verifierSummaryText(state domain.BotChatState, settings domain.BotVerifierSettings) string {
	var b strings.Builder
	b.WriteString("以下是将要提交的申请。点击 ")
	b.WriteString(verifierConfirmButtonText)
	b.WriteString(" 后才会提交给运营人员。\n\n认证方：")
	b.WriteString(strings.TrimSpace(settings.CompanyName))
	b.WriteString("\n对象：")
	b.WriteString(verifierDraftTargetLabel(state))
	b.WriteString("\n将获得的标记：")
	b.WriteString(verifierDescriptionLine(settings))
	b.WriteString("\n\n理由：\n")
	b.WriteString(state.Draft[verifierDraftReason])
	b.WriteString("\n\n这是第三方认证标记，不是官方 ")
	b.WriteString(branding.ProductName())
	b.WriteString(" 认证标记。我不负责决定：运营人员会审核申请并授予或拒绝标记，无论结果如何我都会在这里通知你。")
	return b.String()
}

func verifierFiledText(requestID int64, label string) string {
	return fmt.Sprintf("对象 %s 的申请 #%d 已提交。\n\n申请会由运营人员人工审核，我无法自行授予标记。随时发送 /status 查看状态，审核完成后我会在这里通知你。",
		label, requestID)
}

// ---------------------------------------------------------------------------
// Applicant notifications (botverification.ApplicantNotifier)
// ---------------------------------------------------------------------------

// SendVerificationDecision delivers one operator decision as an ordinary
// @verifierbot message, so it lands in the applicant's history and dialog list like
// any other message. Returning an error keeps the queued notification pending for
// the next cycle.
func (s *Service) SendVerificationDecision(ctx context.Context, recipientUserID int64, req domain.CustomVerificationRequest) error {
	if s == nil || s.messages == nil {
		return fmt.Errorf("custom verification decision: bots service is not configured")
	}
	if recipientUserID <= 0 {
		return fmt.Errorf("custom verification decision: recipient is empty")
	}
	text, ok := verifierDecisionText(req)
	if !ok {
		return fmt.Errorf("custom verification decision: nothing to report for request %d in status %q", req.ID, req.Status)
	}
	mu := s.serviceBotReplyLock(domain.VerifierBotUserID, recipientUserID)
	mu.Lock()
	defer mu.Unlock()
	if _, sent := s.sendServiceBotReplyResult(ctx, domain.VerifierBotUserID, recipientUserID, botReply{Text: text}); !sent {
		return fmt.Errorf("custom verification decision: deliver status %q for request %d", req.Status, req.ID)
	}
	return nil
}

// verifierDecisionText renders one decision for the applicant.
//
// req.InternalNote is NEVER rendered here, under any status: it is the operator's
// private note, kept for the audit trail and the admin panel only. The single field
// that reaches the applicant is req.DecisionReason, which exists precisely because
// it was written to be read by them.
func verifierDecisionText(req domain.CustomVerificationRequest) (string, bool) {
	label := verifierPeerLabel(req.PeerTitle, req.PeerUsername, req.Peer)
	switch req.Status {
	case domain.CustomVerificationApproved:
		return fmt.Sprintf("申请 #%d 已通过：%s 现已显示我的认证标记，名称前会有我的图标，资料中会有我的说明。\n\n这是第三方认证标记，不是官方 %s 认证标记。如需移除，请发送 /revoke。",
			req.ID, label, branding.ProductName()), true
	case domain.CustomVerificationRejected:
		text := fmt.Sprintf("对象 %s 的申请 #%d 未通过，因此没有授予认证标记。", label, req.ID)
		if reason := strings.TrimSpace(req.DecisionReason); reason != "" {
			text += "\n\n原因：" + reason
		}
		return text + "\n\nYou can apply again with /verify once the reason no longer applies.", true
	case domain.CustomVerificationRevoked:
		text := fmt.Sprintf("我的认证标记已从 %s 移除（申请 #%d），图标和资料说明不再显示。", label, req.ID)
		if reason := strings.TrimSpace(req.DecisionReason); reason != "" {
			text += "\n\n原因：" + reason
		}
		return text, true
	default:
		// Pending is not a decision, and an unmodelled status is not one either: report
		// it instead of delivering a message that says nothing.
		return "", false
	}
}

// ---------------------------------------------------------------------------
// Button tokens
// ---------------------------------------------------------------------------

// verifierOptionKeyboard renders one inline keyboard and records its buttons in the
// per-user token table.
//
// SECURITY: callback data never carries a peer id, a peer type, an application id
// or anything else the server would have to trust. Every button's data is
// verifierCallbackDataPrefix plus a freshly generated random token, and that token
// is meaningful only as a key into the token table of *this* applicant's own
// domain.BotChatState. A press is therefore incapable of naming a peer: the server
// resolves the choice from state it wrote itself when it rendered the keyboard, and
// then re-resolves that choice against the applicant's own peers before acting. A
// client that replays or fabricates callback data can at best hit a token absent
// from its own table, which is refused -- so forging a choice for somebody else's
// channel is impossible by construction, not by validation.
//
// Tokens are minted afresh on every render and kept for verifierTokenGenerations
// renders. That window is what makes a second press of the same button resolve to
// the same choice (the handlers are idempotent, so the reply repeats) while the
// table stays bounded.
func (s *Service) verifierOptionKeyboard(state *domain.BotChatState, rows [][]verifierOption) *domain.MessageReplyMarkup {
	if state == nil || len(rows) == 0 {
		return nil
	}
	verifierEnsureDraft(state)
	generation := verifierDraftInt(*state, verifierDraftGeneration) + 1
	state.Draft[verifierDraftGeneration] = strconv.FormatInt(generation, 10)
	verifierPruneOptions(state, generation)
	markup := &domain.MessageReplyMarkup{Type: domain.MessageReplyMarkupInline}
	for _, row := range rows {
		buttons := make([]domain.MarkupButton, 0, len(row))
		for _, option := range row {
			if option.text == "" || option.choice == "" {
				continue
			}
			token := s.verifierOptionToken()
			state.Draft[verifierDraftOptionPrefix+token] = strconv.FormatInt(generation, 10) + "|" + option.choice
			buttons = append(buttons, domain.MarkupButton{
				Type:  domain.MarkupButtonCallback,
				Text:  option.text,
				Style: option.style,
				Data:  []byte(verifierCallbackDataPrefix + token),
			})
		}
		if len(buttons) > 0 {
			markup.Inline = append(markup.Inline, buttons)
		}
	}
	if len(markup.Inline) == 0 {
		return nil
	}
	return markup
}

// verifierOptionToken mints one opaque button token.
func (s *Service) verifierOptionToken() string {
	var buf [verifierOptionTokenBytes]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	// A crypto/rand failure must not brick the dialog. Unpredictability is defence in
	// depth here rather than the load-bearing property -- a token is only ever
	// resolved against the caller's own chat state, and the RPC edge additionally
	// requires the data to appear in a keyboard of a message in the caller's own box
	// -- so a monotonic fallback is safe.
	return strconv.FormatInt(s.now().UnixNano()+s.replySeq.Add(1), 36)
}

// verifierCorrelationID mints the per-attempt idempotency key handed to the
// service with an application.
func (s *Service) verifierCorrelationID(userID int64) string {
	return "verifierbot:" + strconv.FormatInt(userID, 10) + ":" + s.verifierOptionToken()
}

// verifierResolveOption maps callback data back onto the choice recorded for it.
// Anything that is not in this user's own table is refused.
func verifierResolveOption(state domain.BotChatState, data []byte) (string, bool) {
	raw := string(data)
	if !strings.HasPrefix(raw, verifierCallbackDataPrefix) {
		return "", false
	}
	token := raw[len(verifierCallbackDataPrefix):]
	if token == "" || len(token) > verifierOptionTokenMaxLen {
		return "", false
	}
	value, found := state.Draft[verifierDraftOptionPrefix+token]
	if !found {
		return "", false
	}
	_, choice, split := strings.Cut(value, "|")
	if !split || choice == "" {
		return "", false
	}
	return choice, true
}

// verifierPruneOptions drops tokens older than the retained window.
func verifierPruneOptions(state *domain.BotChatState, generation int64) {
	oldest := generation - verifierTokenGenerations + 1
	for key, value := range state.Draft {
		if !strings.HasPrefix(key, verifierDraftOptionPrefix) {
			continue
		}
		rawGen, _, _ := strings.Cut(value, "|")
		gen, err := strconv.ParseInt(rawGen, 10, 64)
		if err != nil || gen < oldest {
			delete(state.Draft, key)
		}
	}
}

// ---------------------------------------------------------------------------
// Service lookups
// ---------------------------------------------------------------------------

// verifierTargetSource resolves the peer enumeration. An explicitly injected
// source wins; otherwise the official verification service is used purely as a
// directory of the applicant's own peers (see verifierBotTargets on why its
// eligibility verdicts are ignored).
func (s *Service) verifierTargetSource() verifierBotTargets {
	if s == nil {
		return nil
	}
	if s.verifierTargets != nil {
		return s.verifierTargets
	}
	if s.verification != nil {
		return s.verification
	}
	return nil
}

// verifierPendingRequest reports the live application on a peer, if any.
func (s *Service) verifierPendingRequest(ctx context.Context, peer domain.Peer) (domain.CustomVerificationRequest, bool) {
	req, err := s.customVerification.PendingRequest(ctx, domain.VerifierBotUserID, peer)
	switch {
	case errors.Is(err, domain.ErrCustomVerificationRequestNotFound):
		return domain.CustomVerificationRequest{}, false
	case err != nil:
		s.log.Warn("verifierbot: read pending request",
			zap.String("peer_type", string(peer.Type)), zap.Int64("peer_id", peer.ID), zap.Error(err))
		return domain.CustomVerificationRequest{}, false
	}
	if req.ID <= 0 || req.Peer != peer || req.Status != domain.CustomVerificationPending {
		return domain.CustomVerificationRequest{}, false
	}
	return req, true
}

// verifierMark reports whether this verifier's mark is live on a peer. The filter
// result is re-checked rather than trusted, so a store that ignores a filter field
// cannot make the bot claim a mark on the wrong peer.
func (s *Service) verifierMark(ctx context.Context, peer domain.Peer) (domain.CustomVerification, bool) {
	marks, err := s.customVerification.Marks(ctx, domain.CustomVerificationFilter{
		VerifierBotID: domain.VerifierBotUserID,
		PeerType:      peer.Type,
		PeerID:        peer.ID,
		Limit:         1,
	})
	if err != nil {
		s.log.Warn("verifierbot: read mark",
			zap.String("peer_type", string(peer.Type)), zap.Int64("peer_id", peer.ID), zap.Error(err))
		return domain.CustomVerification{}, false
	}
	for _, mark := range marks {
		if mark.VerifierBotID == domain.VerifierBotUserID && mark.Peer == peer {
			return mark, true
		}
	}
	return domain.CustomVerification{}, false
}

// ---------------------------------------------------------------------------
// State helpers
// ---------------------------------------------------------------------------

func verifierNewState(userID int64) domain.BotChatState {
	return domain.BotChatState{
		BotUserID: domain.VerifierBotUserID,
		UserID:    userID,
		Command:   verifierBotCommand,
		Step:      verifierStepIntro,
		Draft:     map[string]string{},
	}
}

func verifierEnsureDraft(state *domain.BotChatState) {
	if state.Draft == nil {
		state.Draft = map[string]string{}
	}
	if state.BotUserID == 0 {
		state.BotUserID = domain.VerifierBotUserID
	}
	if state.Command == "" {
		state.Command = verifierBotCommand
	}
}

func (s *Service) saveVerifierState(ctx context.Context, state domain.BotChatState) bool {
	verifierEnsureDraft(&state)
	state.BotUserID = domain.VerifierBotUserID
	clone := domain.BotChatState{
		BotUserID: state.BotUserID,
		UserID:    state.UserID,
		Command:   state.Command,
		Step:      state.Step,
		Draft:     make(map[string]string, len(state.Draft)),
	}
	for key, value := range state.Draft {
		clone.Draft[key] = value
	}
	if err := s.bots.UpsertBotChatState(ctx, clone); err != nil {
		s.log.Error("verifierbot: save chat state", zap.Int64("user_id", state.UserID), zap.Error(err))
		return false
	}
	return true
}

func (s *Service) deleteVerifierState(ctx context.Context, userID int64) {
	if err := s.bots.DeleteBotChatState(ctx, domain.VerifierBotUserID, userID); err != nil {
		s.log.Error("verifierbot: delete chat state", zap.Int64("user_id", userID), zap.Error(err))
	}
}

func verifierDraftInt(state domain.BotChatState, key string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(state.Draft[key]), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// verifierDraftTargetOf reads back the chosen subject. The stored form is the
// verification target kind plus its id, so the peer namespace is derived by the
// domain rather than remembered separately.
func verifierDraftTargetOf(state domain.BotChatState) (domain.VerificationTarget, bool) {
	kind, rawID, split := strings.Cut(state.Draft[verifierDraftTarget], ":")
	if !split {
		return domain.VerificationTarget{}, false
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	targetType := domain.VerificationTargetType(kind)
	if err != nil || id <= 0 || !targetType.Valid() {
		return domain.VerificationTarget{}, false
	}
	return domain.VerificationTarget{
		Type:     targetType,
		ID:       id,
		Title:    state.Draft[verifierDraftTargetTitle],
		Username: state.Draft[verifierDraftTargetUsername],
	}, true
}

func verifierDraftTargetLabel(state domain.BotChatState) string {
	target, ok := verifierDraftTargetOf(state)
	if !ok {
		return "已选择的对象"
	}
	return verifierPeerLabel(target.Title, target.Username, verifierTargetPeer(target))
}

// verifierPeerKey renders a peer for the token table: the two peer namespaces a
// mark can live in, and an id. It never reaches a client -- only the random token
// standing for it does.
func verifierPeerKey(peer domain.Peer) string {
	return string(peer.Type) + ":" + strconv.FormatInt(peer.ID, 10)
}

// verifierParsePeer reads a verifierPeerKey back. It accepts only the two peer
// namespaces a mark can live in.
func verifierParsePeer(payload string) (domain.Peer, bool) {
	kind, rawID, split := strings.Cut(payload, ":")
	if !split {
		return domain.Peer{}, false
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		return domain.Peer{}, false
	}
	peerType := domain.PeerType(kind)
	if peerType != domain.PeerTypeUser && peerType != domain.PeerTypeChannel {
		return domain.Peer{}, false
	}
	return domain.Peer{Type: peerType, ID: id}, true
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

// verifierPeerLabel renders a subject for humans. The username is the identity
// that matters, the title is the context, and the peer is the last resort so a
// message never says "the selected peer" when it can name one.
func verifierPeerLabel(title, username string, peer domain.Peer) string {
	title = strings.TrimSpace(title)
	username = strings.TrimSpace(username)
	switch {
	case username != "" && title != "":
		return verifierTruncate(title, 64) + " (@" + username + ")"
	case username != "":
		return "@" + username
	case title != "":
		return verifierTruncate(title, 64)
	case peer.Type == domain.PeerTypeChannel && peer.ID > 0:
		return "频道 " + strconv.FormatInt(peer.ID, 10)
	case peer.Type == domain.PeerTypeUser && peer.ID > 0:
		return "账号 " + strconv.FormatInt(peer.ID, 10)
	default:
		return "已选择的对象"
	}
}

func verifierTargetButtonText(target domain.VerificationTarget) string {
	label := strings.TrimSpace(target.Username)
	if label != "" {
		label = "@" + label
	} else {
		label = strings.TrimSpace(target.Title)
	}
	if label == "" {
		label = "id " + strconv.FormatInt(target.ID, 10)
	}
	return verifierTargetKindLabel(target.Type) + ": " + verifierTruncate(label, 64)
}

func verifierTargetKindLabel(kind domain.VerificationTargetType) string {
	switch kind {
	case domain.VerificationTargetBot:
		return "机器人"
	case domain.VerificationTargetChannel:
		return "频道"
	case domain.VerificationTargetSupergroup:
		return "群组"
	case domain.VerificationTargetUser:
		return "账号"
	default:
		return "对象"
	}
}

func verifierStatusLabel(status domain.CustomVerificationRequestStatus) string {
	switch status {
	case domain.CustomVerificationPending:
		return "等待运营人员审核"
	case domain.CustomVerificationApproved:
		return "已通过，认证标记已授予"
	case domain.CustomVerificationRejected:
		return "未通过"
	case domain.CustomVerificationRevoked:
		return "认证标记已移除"
	default:
		return string(status)
	}
}

func verifierDateLabel(req domain.CustomVerificationRequest) string {
	switch {
	case !req.ApprovedAt.IsZero():
		return "授予于 " + req.ApprovedAt.UTC().Format("2006-01-02")
	case !req.RejectedAt.IsZero():
		return "决定于 " + req.RejectedAt.UTC().Format("2006-01-02")
	case !req.CreatedAt.IsZero():
		return "提交于 " + req.CreatedAt.UTC().Format("2006-01-02")
	default:
		return ""
	}
}

func verifierJoin(lead, body string) string {
	lead = strings.TrimSpace(lead)
	if lead == "" {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return lead
	}
	return lead + "\n\n" + body
}

func verifierTruncate(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}

// ---------------------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------------------

func (s *Service) verifierErrorReply(userID int64, operation string, err error) botReply {
	if text, ok := verifierPolicyText(err); ok {
		return botReply{Text: text}
	}
	s.log.Error("verifierbot: "+operation, zap.Int64("user_id", userID), zap.Error(err))
	return internalReply()
}

// verifierPolicyText turns a policy refusal into a sentence the applicant can act
// on. Anything that is not a policy error is an internal fault and is logged
// instead of being narrated.
func verifierPolicyText(err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, domain.ErrVerifierNotFound), errors.Is(err, domain.ErrVerifierForbidden),
		errors.Is(err, domain.ErrVerifierSettingsInvalid):
		return verifierNotActivatedText, true
	case errors.Is(err, domain.ErrVerificationIconNotFound), errors.Is(err, domain.ErrVerificationIconInactive),
		errors.Is(err, domain.ErrVerificationIconInvalid):
		return "我的认证图标目前不可用，因此无法添加任何标记。运营人员修复后才能接受申请。", true
	case errors.Is(err, domain.ErrCustomVerificationLimit):
		return "我已经达到允许添加标记的对象数量上限，运营人员提高上限后才能接受新申请。", true
	case errors.Is(err, domain.ErrCustomVerificationRequestExists):
		return "该对象已有等待处理的申请，请发送 /status 查看。", true
	case errors.Is(err, domain.ErrCustomVerificationTargetInvalid):
		return "该对象无法添加我的认证标记。我可以认证机器人、频道、超级群组或账号。", true
	case errors.Is(err, domain.ErrCustomVerificationNotFound):
		return "该对象上没有我的认证标记，因此没有可移除的内容。", true
	case errors.Is(err, domain.ErrCustomVerificationRequestNotFound):
		return "找不到该申请，请发送 /verify 提交新申请。", true
	case errors.Is(err, domain.ErrCustomVerificationRequestInvalid):
		return "无法接受该内容，请发送 /help 查看申请要求。", true
	case errors.Is(err, domain.ErrCustomVerificationVersionConflict):
		return "该申请刚刚发生变化，请发送 /status 查看最新状态。", true
	case errors.Is(err, domain.ErrVerifierDescriptionForbidden):
		return "我只能使用自己的认证说明，不能为你的对象使用自定义说明。", true
	default:
		return "", false
	}
}
