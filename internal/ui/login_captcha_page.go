package ui

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anhoder/foxful-cli/model"
	"github.com/anhoder/foxful-cli/util"
	"github.com/buger/jsonparser"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/go-musicfox/go-musicfox/internal/configs"
	"github.com/go-musicfox/go-musicfox/utils/app"
	"github.com/go-musicfox/go-musicfox/utils/slogx"
	"github.com/go-musicfox/netease-music/service"
	neteaseUtil "github.com/go-musicfox/netease-music/util"
	cookiejar "github.com/juju/persistent-cookiejar"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

const LoginCaptchaPageType model.PageType = "login_captcha"

const (
	captchaSubmitIndex = 2 // skip phone and captcha input
	sendCaptchaIndex   = 3
)

type tickCaptchaMsg struct{}
type tickCaptchaRenderMsg struct{}

func tickCaptcha(duration time.Duration) tea.Cmd {
	return tea.Tick(duration, func(t time.Time) tea.Msg {
		return tickCaptchaMsg{}
	})
}

func tickCaptchaRender(duration time.Duration) tea.Cmd {
	return tea.Tick(duration, func(t time.Time) tea.Msg {
		return tickCaptchaRenderMsg{}
	})
}

type LoginCaptchaPage struct {
	netease *Netease
	from    model.Page

	menuTitle    *model.MenuItem
	index        int
	phoneInput   textinput.Model
	captchaInput textinput.Model
	submitButton string
	sendButton   string
	countDown    int
	tips         string
	AfterLogin   LoginCallback

	// UI layout positions
	phoneRowY    int
	captchaRowY  int
	buttonsRowY  int
	submitStartX int
	submitEndX   int
	sendStartX   int
	sendEndX     int
}

func NewLoginCaptchaPage(netease *Netease, from model.Page, afterLogin LoginCallback) *LoginCaptchaPage {
	phoneInput := textinput.New()
	phoneInput.Placeholder = " 手机号"
	phoneInput.Focus()
	phoneInput.Prompt = model.GetFocusedPrompt()
	phoneInput.TextStyle = util.GetPrimaryFontStyle()
	phoneInput.CharLimit = 15

	captchaInput := textinput.New()
	captchaInput.Placeholder = " 验证码"
	captchaInput.Prompt = "> "
	captchaInput.CharLimit = 6

	page := &LoginCaptchaPage{
		netease:      netease,
		from:         from,
		AfterLogin:   afterLogin,
		menuTitle:    &model.MenuItem{Title: "用户登录", Subtitle: "验证码登录"},
		phoneInput:   phoneInput,
		captchaInput: captchaInput,
		submitButton: model.GetBlurredSubmitButton(),
		sendButton:   model.GetBlurredButton("发送验证码"),
	}
	return page
}

func (l *LoginCaptchaPage) IgnoreQuitKeyMsg(_ tea.KeyMsg) bool {
	return true
}

func (l *LoginCaptchaPage) Type() model.PageType {
	return LoginCaptchaPageType
}

func (l *LoginCaptchaPage) Update(msg tea.Msg, _ *model.App) (model.Page, tea.Cmd) {
	var (
		key tea.KeyMsg
		ok  bool
	)

	if _, ok = msg.(tickCaptchaMsg); ok {
		if l.countDown > 0 {
			l.countDown--
			l.updateSendButton()
			if l.countDown > 0 {
				return l, tickCaptcha(time.Second)
			}
		}
		return l, nil
	}

	if _, ok = msg.(tickCaptchaRenderMsg); ok {
		return l, nil
	}

	if mouse, ok := msg.(tea.MouseMsg); ok {
		if mouse.Button == tea.MouseButtonLeft && mouse.Action == tea.MouseActionPress {
			y := mouse.Y + 1
			x := mouse.X

			if y == l.phoneRowY {
				l.index = 0
				l.focusInputs()
				return l, tickCaptchaRender(time.Nanosecond)
			}
			if y == l.captchaRowY {
				l.index = 1
				l.focusInputs()
				return l, tickCaptchaRender(time.Nanosecond)
			}

			if y == l.buttonsRowY {
				if x >= l.submitStartX && x <= l.submitEndX {
					l.index = captchaSubmitIndex
					return l.enterHandler()
				}
				if x >= l.sendStartX && x <= l.sendEndX {
					l.index = sendCaptchaIndex
					return l.enterHandler()
				}
			}
		}
	}

	if key, ok = msg.(tea.KeyMsg); !ok {
		return l, tickCaptchaRender(time.Nanosecond)
	}

	switch key.String() {
	case "esc":
		return l.from, tickCaptchaRender(time.Nanosecond)
	case "tab":
		l.index = (l.index + 1) % 4
		l.focusInputs()
	case "shift+tab":
		l.index = (l.index - 1 + 4) % 4
		l.focusInputs()
	case "enter":
		return l.enterHandler()
	}

	switch l.index {
	case 0:
		l.phoneInput, _ = l.phoneInput.Update(msg)
	case 1:
		l.captchaInput, _ = l.captchaInput.Update(msg)
	}

	return l, tickCaptchaRender(time.Nanosecond)
}

func (l *LoginCaptchaPage) focusInputs() {
	setFocused := func(input *textinput.Model) {
		input.Focus()
		input.Prompt = model.GetFocusedPrompt()
		input.TextStyle = util.GetPrimaryFontStyle()
	}
	setBlurred := func(input *textinput.Model) {
		input.Blur()
		input.Prompt = "> "
		input.TextStyle = lipgloss.NewStyle()
	}

	setBlurred(&l.phoneInput)
	setBlurred(&l.captchaInput)
	setSubmitFocused := func(focused bool) {
		if focused {
			l.submitButton = model.GetFocusedSubmitButton()
			return
		}
		l.submitButton = model.GetBlurredSubmitButton()
	}

	switch l.index {
	case 0:
		setFocused(&l.phoneInput)
		setSubmitFocused(false)
	case 1:
		setFocused(&l.captchaInput)
		setSubmitFocused(false)
	case captchaSubmitIndex:
		setSubmitFocused(true)
	case sendCaptchaIndex:
		setSubmitFocused(false)
	}

	l.updateSendButton()
}

func (l *LoginCaptchaPage) updateSendButton() {
	label := "发送验证码"
	if l.countDown > 0 {
		label = fmt.Sprintf("%ds", l.countDown)
		label = string(util.SetFgStyle(label, termenv.ANSIBrightBlack))
	}
	if l.index == sendCaptchaIndex && l.countDown == 0 {
		l.sendButton = model.GetFocusedButton(label)
		return
	}
	l.sendButton = model.GetBlurredButton(label)
}

func (l *LoginCaptchaPage) enterHandler() (model.Page, tea.Cmd) {
	switch l.index {
	case sendCaptchaIndex:
		// 发送验证码
		if l.countDown > 0 {
			return l, nil
		}

		phone := strings.TrimSpace(l.phoneInput.Value())
		if phone == "" {
			l.tips = util.SetFgStyle("请输入手机号", termenv.ANSIBrightRed)
			return l, nil
		}

		code, response := l.captchaServiceRequest("send", phone, "", "86")

		if code != 200 {
			slog.Error("发送验证码失败", slog.String("response", string(response)))
			msg, _ := jsonparser.GetString(response, "message")
			if msg == "" {
				msg = "发送验证码失败，请重试"
			} else {
				msg = "API: " + msg
			}
			l.tips = util.SetFgStyle(msg, termenv.ANSIBrightRed)
		} else {
			l.tips = util.SetFgStyle("验证码已发送", termenv.ANSIBrightGreen)
			l.countDown = 30
			l.sendButton = model.GetBlurredButton("30s")
			return l, tickCaptcha(time.Second)
		}
		return l, nil
	case captchaSubmitIndex:
		// 验证并登录
		phone := strings.TrimSpace(l.phoneInput.Value())
		captcha := strings.TrimSpace(l.captchaInput.Value())

		if phone == "" || captcha == "" {
			l.tips = util.SetFgStyle("请输入手机号和验证码", termenv.ANSIBrightRed)
			return l, nil
		}

		l.tips = util.SetFgStyle("正在登录...", termenv.ANSIBrightYellow)

		// 清理旧 Cookie 并创建新的 CookieJar
		dataDir := app.DataDir()
		cookieFile := filepath.Join(dataDir, "cookie")
		if err := os.Remove(cookieFile); err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to remove cookie file", slogx.Error(err))
		}
		jar, err := cookiejar.New(&cookiejar.Options{
			Filename: cookieFile,
		})
		if err != nil {
			slog.Error("Failed to create cookie jar", slogx.Error(err))
		} else {
			neteaseUtil.SetGlobalCookieJar(jar)

			// 生成并设置 deviceId 和 sDeviceId
			deviceId := neteaseUtil.GenerateSDeviceId()
			sDeviceId := deviceId // 使用相同的 ID
			targetUrl, _ := url.Parse("https://music.163.com")
			jar.SetCookies(targetUrl, []*http.Cookie{
				{Name: "deviceId", Value: deviceId},
				{Name: "sDeviceId", Value: sDeviceId},
			})
		}

		// Verify Captcha
		code, response := l.captchaServiceRequest("verify", phone, captcha, "86")
		if code != 200 {
			slog.Error("验证码错误", slog.String("response", string(response)))
			l.tips = util.SetFgStyle("验证码错误或已过期", termenv.ANSIBrightRed)
			return l, nil
		}

		// Login
		code, response = l.captchaServiceRequest("login", phone, captcha, "86")

		if code == 200 {
			l.tips = ""
			if newPage := l.loginSuccessHandle(l.netease); newPage != nil {
				return newPage, l.netease.Tick(time.Nanosecond)
			}
			return l.netease.MustMain(), model.TickMain(time.Nanosecond)
		} else {
			slog.Error("登录失败", slog.String("response", string(response)))
			msg, _ := jsonparser.GetString(response, "message")
			if msg == "" {
				msg = "登录失败，请重试"
			}
			l.tips = util.SetFgStyle(msg, termenv.ANSIBrightRed)
		}
		return l, nil
	}

	l.index = (l.index + 1) % 4
	l.focusInputs()
	return l, tickCaptchaRender(time.Nanosecond)
}

func (l *LoginCaptchaPage) View(a *model.App) string {
	var (
		builder  strings.Builder
		top      int // 距离顶部的行数
		mainPage = l.netease.MustMain()
	)

	lineCount := 0
	write := func(s string) {
		builder.WriteString(s)
		lineCount += strings.Count(s, "\n")
	}
	curRow := func() int {
		return lineCount + 1 // 1-based
	}

	// title
	if configs.AppConfig.Theme.ShowTitle {
		write(mainPage.TitleView(a, &top))
	} else {
		top++
	}

	// menu title
	write(mainPage.MenuTitleView(a, &top, l.menuTitle))
	write("\n")
	top++

	write("\n\n")
	top += 2

	// Phone Input
	if mainPage.MenuStartColumn() > 0 {
		write(strings.Repeat(" ", mainPage.MenuStartColumn()))
	}
	l.phoneRowY = curRow()
	write(l.phoneInput.View())
	write("\n\n")
	top += 2

	// Captcha Input
	if mainPage.MenuStartColumn() > 0 {
		write(strings.Repeat(" ", mainPage.MenuStartColumn()))
	}
	l.captchaRowY = curRow()
	write(l.captchaInput.View())
	write("\n\n")
	top += 2

	// Buttons
	if mainPage.MenuStartColumn() > 0 {
		write(strings.Repeat(" ", mainPage.MenuStartColumn()))
	}
	l.buttonsRowY = curRow()

	// Send Button
	submitX := mainPage.MenuStartColumn()
	if submitX < 0 {
		submitX = 0
	}
	l.sendStartX = submitX
	write(l.sendButton)
	l.sendEndX = l.sendStartX + runewidth.StringWidth(l.sendButton)

	write("   ") // Spacing

	// Submit Button
	l.submitStartX = l.sendEndX + 3
	write(l.submitButton)
	l.submitEndX = l.submitStartX + runewidth.StringWidth(l.submitButton)

	write("\n\n\n")

	// Tips
	if l.tips != "" {
		if mainPage.MenuStartColumn() > 0 {
			write(strings.Repeat(" ", mainPage.MenuStartColumn()))
		}
		builder.WriteString(l.tips)
	}

	return builder.String()
}

func (l *LoginCaptchaPage) Msg() tea.Msg {
	return tickLoginMsg{}
}

// loginSuccessHandle 登录成功处理函数
func (l *LoginCaptchaPage) loginSuccessHandle(n *Netease) model.Page {
	if err := n.LoginCallback(); err != nil {
		slog.Error("login callback error", slogx.Error(err))
	}

	var newPage model.Page
	if l.AfterLogin != nil {
		newPage = l.AfterLogin()
	}
	return newPage
}

// captchaServiceRequest 统一处理验证码相关请求
func (l *LoginCaptchaPage) captchaServiceRequest(action, phone, captcha, ct string) (code float64, response []byte) {
	switch action {
	case "send":
		s := service.CaptchaSentService{Cellphone: phone, Ctcode: ct}
		return s.CaptchaSent()
	case "verify":
		s := service.CaptchaVerifyService{Cellphone: phone, Captcha: captcha, Ctcode: ct}
		return s.CaptchaVerify()
	case "login":
		s := service.LoginCellphoneService{Phone: phone, Captcha: captcha, Countrycode: ct}
		code, response, _ = s.LoginCellphone()
		return code, response
	}
	return 0, nil
}
