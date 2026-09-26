package bots

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	telegramloginapp "telesrv/internal/app/telegramlogin"
	"telesrv/internal/domain"
	"telesrv/internal/store/memory"
)

func newBotFatherTelegramLoginService(t *testing.T) *telegramloginapp.Service {
	t.Helper()
	sealKey := make([]byte, 32)
	sealKey[0] = 1
	sealer, err := telegramloginapp.NewCodeSealer("test", map[string][]byte{"test": sealKey})
	if err != nil {
		t.Fatal(err)
	}
	pepper := make([]byte, 32)
	pepper[0] = 2
	service, err := telegramloginapp.NewService(memory.NewTelegramLoginStore(nil), sealer, telegramloginapp.Config{
		Issuer: "http://192.0.2.25:2404", AppScheme: "telesrv", AllowHTTP: true,
		ClientSecretPepper: pepper, Now: func() time.Time { return time.Unix(1_780_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestBotFatherTelegramLoginConfigurationFlow(t *testing.T) {
	svc, users, bots, messages := newTestService(t)
	svc.telegramLogin = newBotFatherTelegramLoginService(t)
	owner := newOwner(t, users, "+1090")
	bot, _, err := svc.CreateBotWithDelivery(context.Background(), owner.ID, "Login Demo", "login_demo_bot", appBotLifecycleEffects)
	if err != nil {
		t.Fatal(err)
	}

	if reply := sendToBotFather(t, svc, messages, owner, "/setlogin"); !strings.Contains(reply, "请选择") {
		t.Fatalf("/setlogin reply = %q", reply)
	}
	created := sendToBotFather(t, svc, messages, owner, "@login_demo_bot")
	if !strings.Contains(created, "客户端 ID："+strconv.FormatInt(bot.ID, 10)) || !strings.Contains(created, "只会显示一次") {
		t.Fatalf("create login reply = %q", created)
	}
	secretMarker := "只会显示一次：\n"
	secret := strings.SplitN(strings.SplitN(created, secretMarker, 2)[1], "\n", 2)[0]
	if len(secret) < 32 {
		t.Fatalf("client secret is unexpectedly short: %q", secret)
	}
	if reply := sendToBotFather(t, svc, messages, owner, "add origin http://rp.example.test:3000"); !strings.Contains(reply, "成功") {
		t.Fatalf("add origin reply = %q", reply)
	}
	state, found, err := bots.GetBotChatState(context.Background(), domain.BotFatherUserID, owner.ID)
	if err != nil || !found || state.Step != botFatherStepValue || state.Draft[botFatherDraftBotID] != strconv.FormatInt(bot.ID, 10) {
		t.Fatalf("state after first command = %+v, found=%v err=%v", state, found, err)
	}
	if reply := sendToBotFather(t, svc, messages, owner, "add redirect http://192.0.2.26:3000/auth/callback"); !strings.Contains(reply, "成功") {
		t.Fatalf("add redirect reply = %q", reply)
	}
	if reply := sendToBotFather(t, svc, messages, owner, "algorithm ES256"); !strings.Contains(reply, "ES256") {
		t.Fatalf("algorithm reply = %q", reply)
	}
	if reply := sendToBotFather(t, svc, messages, owner, "add ios dev.bedolaga.demo ABCDE12345 bedolaga://telegram-login Bedolaga iOS Demo"); !strings.Contains(reply, "注册原生应用 #") {
		t.Fatalf("add iOS app reply = %q", reply)
	}
	fingerprint := strings.Repeat("A", 64)
	if reply := sendToBotFather(t, svc, messages, owner, "add android dev.bedolaga.demo "+fingerprint+" bedolaga://android-login Bedolaga Android Demo"); !strings.Contains(reply, "注册原生应用 #") {
		t.Fatalf("add Android app reply = %q", reply)
	}
	done := sendToBotFather(t, svc, messages, owner, "/done")
	if !strings.Contains(done, "配置已完成") || !strings.Contains(done, "签名算法：ES256") {
		t.Fatalf("/done reply = %q", done)
	}
	if _, found, err := bots.GetBotChatState(context.Background(), domain.BotFatherUserID, owner.ID); err != nil || found {
		t.Fatalf("state after /done: found=%v err=%v", found, err)
	}

	sendToBotFather(t, svc, messages, owner, "/logininfo")
	info := sendToBotFather(t, svc, messages, owner, "login_demo_bot")
	for _, want := range []string{"签名算法：ES256", "web_origin：http://rp.example.test:3000", "redirect_uri：http://192.0.2.26:3000/auth/callback", "dev.bedolaga.demo", "Bedolaga iOS Demo", "Bedolaga Android Demo"} {
		if !strings.Contains(info, want) {
			t.Fatalf("login info = %q, missing %q", info, want)
		}
	}
	if strings.Contains(info, secret) {
		t.Fatal("/logininfo leaked the one-time client secret")
	}

	sendToBotFather(t, svc, messages, owner, "/resetloginsecret")
	rotated := sendToBotFather(t, svc, messages, owner, "login_demo_bot")
	if !strings.Contains(rotated, "旧 OIDC 客户端密钥") || strings.Contains(rotated, secret) {
		t.Fatalf("rotate reply = %q", rotated)
	}
}

func TestBotFatherTelegramLoginBatchAndCancelFlow(t *testing.T) {
	svc, users, bots, messages := newTestService(t)
	svc.telegramLogin = newBotFatherTelegramLoginService(t)
	owner := newOwner(t, users, "+1091")
	bot, _, err := svc.CreateBotWithDelivery(context.Background(), owner.ID, "Batch Login Demo", "batch_login_bot", appBotLifecycleEffects)
	if err != nil {
		t.Fatal(err)
	}

	if reply := sendToBotFather(t, svc, messages, owner, "/done"); !strings.Contains(reply, "当前没有正在进行") {
		t.Fatalf("inactive /done reply = %q", reply)
	}
	sendToBotFather(t, svc, messages, owner, "/setlogin")
	sendToBotFather(t, svc, messages, owner, "@batch_login_bot")
	tooMany := strings.TrimSuffix(strings.Repeat("enable\n", maxTelegramLoginCommandsPerMessage+1), "\n")
	if reply := sendToBotFather(t, svc, messages, owner, tooMany); !strings.Contains(reply, "每次最多发送 32 行") {
		t.Fatalf("oversized batch reply = %q", reply)
	}
	oversizedConfiguration, found, err := svc.telegramLogin.ClientConfiguration(context.Background(), bot.ID)
	if err != nil || !found || len(oversizedConfiguration.AllowedURLs) != 0 || oversizedConfiguration.Client.SigningAlgorithm != "RS256" {
		t.Fatalf("configuration after oversized batch = %+v, found=%v err=%v", oversizedConfiguration, found, err)
	}
	batch := strings.Join([]string{
		"add origin http://batch.example.test:3000",
		"add redirect http://batch.example.test:3000/auth/telegram/callback",
		"algorithm ES256",
		"enable",
	}, "\n")
	if reply := sendToBotFather(t, svc, messages, owner, batch); !strings.Contains(reply, "已应用全部 4 条命令") || !strings.Contains(reply, "/done") {
		t.Fatalf("batch reply = %q", reply)
	}
	configuration, found, err := svc.telegramLogin.ClientConfiguration(context.Background(), bot.ID)
	if err != nil || !found || !configuration.Client.Enabled || configuration.Client.SigningAlgorithm != "ES256" || len(configuration.AllowedURLs) != 2 {
		t.Fatalf("configuration after batch = %+v, found=%v err=%v", configuration, found, err)
	}

	partial := strings.Join([]string{
		"add origin http://second.example.test:3001",
		"add redirect not-a-url",
		"disable",
	}, "\n")
	partialReply := sendToBotFather(t, svc, messages, owner, partial)
	for _, want := range []string{"发生错误前已应用 1 条命令", "已在第 2 行停止", "后续 1 条命令未应用", "/done"} {
		if !strings.Contains(partialReply, want) {
			t.Fatalf("partial batch reply = %q, missing %q", partialReply, want)
		}
	}
	configuration, found, err = svc.telegramLogin.ClientConfiguration(context.Background(), bot.ID)
	if err != nil || !found || !configuration.Client.Enabled || len(configuration.AllowedURLs) != 3 {
		t.Fatalf("configuration after partial batch = %+v, found=%v err=%v", configuration, found, err)
	}
	if _, found, err := bots.GetBotChatState(context.Background(), domain.BotFatherUserID, owner.ID); err != nil || !found {
		t.Fatalf("state after partial batch: found=%v err=%v", found, err)
	}

	if reply := sendToBotFather(t, svc, messages, owner, "/cancel"); !strings.Contains(reply, "之前已经应用的更改会保留") {
		t.Fatalf("/cancel reply = %q", reply)
	}
	if _, found, err := bots.GetBotChatState(context.Background(), domain.BotFatherUserID, owner.ID); err != nil || found {
		t.Fatalf("state after /cancel: found=%v err=%v", found, err)
	}
	configuration, found, err = svc.telegramLogin.ClientConfiguration(context.Background(), bot.ID)
	if err != nil || !found || len(configuration.AllowedURLs) != 3 {
		t.Fatalf("configuration after /cancel = %+v, found=%v err=%v", configuration, found, err)
	}
}
