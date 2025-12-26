package player

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aynakeya/go-mpv"
	"github.com/go-musicfox/go-musicfox/internal/types"
	"github.com/go-musicfox/go-musicfox/utils/slogx"
)

// mpvPlayer 实现基于MPV的播放器
type mpvPlayer struct {
	handle *mpv.Mpv // MPV handle

	mutex sync.Mutex

	curMusic URLMusic

	volume        int
	state         types.State
	timeChan      chan time.Duration
	stateChan     chan types.State
	close         chan struct{}
	cachedTimePos time.Duration // 缓存的播放位置，避免频繁查询
	lastSyncTime  time.Time     // 上次同步时间
	ticker        *time.Ticker  // 定期同步播放进度
	tickerDone    chan bool     // ticker 停止信号
	playStartTime time.Time     // 播放开始时间
	eventDone     chan bool     // event listener 停止信号
}

// NewMpvPlayer 创建新的MPV播放器实例
func NewMpvPlayer() *mpvPlayer {
	// 创建 MPV handle
	handle := mpv.Create()
	if handle == nil {
		panic("无法创建 MPV handle")
	}

	p := &mpvPlayer{
		handle:    handle,
		volume:    50, // 默认音量
		state:     types.Stopped,
		timeChan:  make(chan time.Duration),
		stateChan: make(chan types.State),
		close:     make(chan struct{}),
		eventDone: make(chan bool),
	}

	// 配置 MPV 选项（在 Initialize 之前）
	_ = handle.SetOptionString("video", "no")          // 无视频模式
	_ = handle.SetOptionString("terminal", "no")       // 不使用终端
	_ = handle.SetOptionString("cache", "yes")         // 启用缓存
	_ = handle.SetOptionString("audio-device", "auto") // 自动选择音频设备
	_ = handle.SetOption("volume", mpv.FORMAT_INT64, int64(p.volume))
	_ = handle.SetOptionString("demuxer-max-bytes", "120MiB")   // 增大缓存容量
	_ = handle.SetOptionString("demuxer-readahead-secs", "120") // 增加预读时间

	// 初始化 MPV
	if err := handle.Initialize(); err != nil {
		handle.Destroy()
		panic(fmt.Sprintf("MPV初始化失败: %v", err))
	}

	// 启动事件监听
	go p.listenMpvEvent()

	return p
}

func buildMpvMediaTitle(music URLMusic) string {
	name := strings.TrimSpace(music.Name)
	if name == "" {
		return ""
	}

	var artists []string
	for _, a := range music.Artists {
		an := strings.TrimSpace(a.Name)
		if an != "" {
			artists = append(artists, an)
		}
	}
	if len(artists) == 0 {
		return sanitizeMpvTitle(name)
	}
	return sanitizeMpvTitle(name + " - " + strings.Join(artists, ", "))
}

func sanitizeMpvTitle(title string) string {
	title = strings.ReplaceAll(title, "\n", " ")
	title = strings.ReplaceAll(title, "\r", " ")
	return strings.TrimSpace(title)
}

// 监听mpv事件
func (p *mpvPlayer) listenMpvEvent() {
	for {
		select {
		case <-p.eventDone:
			return
		default:
			// 等待事件，超时1秒
			event := p.handle.WaitEvent(1.0)
			if event == nil {
				continue
			}

			switch event.EventId {
			case mpv.EVENT_END_FILE:
				// 歌曲播放结束
				p.setState(types.Stopped)
				p.mutex.Lock()
				p.stopTicker()
				p.mutex.Unlock()
			case mpv.EVENT_PLAYBACK_RESTART:
				// 播放重启（例如seek后）
				// 可以在这里处理状态更新
			}
		}
	}
}

// Play 播放指定音乐
func (p *mpvPlayer) Play(music URLMusic) {
	// 重置播放状态（保护 curMusic、ticker 和 state）
	p.mutex.Lock()
	p.curMusic = music
	p.stopTicker()
	p.lastSyncTime = time.Now()
	p.playStartTime = time.Now()
	p.cachedTimePos = 0
	p.startTicker()
	p.state = types.Playing
	p.mutex.Unlock()

	// 加载并播放音乐
	mediaTitle := buildMpvMediaTitle(music)
	if mediaTitle != "" {
		_ = p.handle.SetPropertyString("force-media-title", mediaTitle)
	}

	if err := p.handle.Command([]string{"loadfile", music.URL}); err != nil {
		slog.Error("MPV播放失败", slogx.Error(err))
		// 如果加载失败，需要恢复状态
		p.mutex.Lock()
		p.state = types.Stopped
		p.stopTicker()
		p.mutex.Unlock()
		return
	}

	// 通知状态变化
	select {
	case p.stateChan <- types.Playing:
	case <-time.After(time.Second * 2):
	}
}

// startTicker 启动定期同步 ticker
func (p *mpvPlayer) startTicker() {
	p.ticker = time.NewTicker(200 * time.Millisecond)
	p.tickerDone = make(chan bool)

	go func() {
		for {
			select {
			case <-p.ticker.C:
				// 每秒从 mpv 同步一次实际播放位置
				if time.Since(p.lastSyncTime) >= time.Second {
					if timePos, err := p.getMpvTimePos(); err == nil {
						p.mutex.Lock()
						p.cachedTimePos = timePos
						p.lastSyncTime = time.Now()
						p.mutex.Unlock()
					}
				}
				// 发送当前播放时间
				select {
				case p.timeChan <- p.PassedTime():
				default:
				}
			case <-p.tickerDone:
				return
			}
		}
	}()
}

// stopTicker 停止 ticker
func (p *mpvPlayer) stopTicker() {
	if p.ticker != nil {
		p.ticker.Stop()
		if p.tickerDone != nil {
			close(p.tickerDone)
			p.tickerDone = nil
		}
		p.ticker = nil
	}
}

// getMpvTimePos 从MPV获取当前播放位置（秒）
func (p *mpvPlayer) getMpvTimePos() (time.Duration, error) {
	val, err := p.handle.GetProperty("time-pos", mpv.FORMAT_DOUBLE)
	if err != nil {
		return 0, err
	}
	seconds, ok := val.(float64)
	if !ok {
		return 0, fmt.Errorf("time-pos 类型错误")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// CurMusic 获取当前播放的音乐
func (p *mpvPlayer) CurMusic() URLMusic {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.curMusic
}

// Pause 暂停播放
func (p *mpvPlayer) Pause() {
	p.mutex.Lock()
	if p.state != types.Playing {
		p.mutex.Unlock()
		return
	}
	p.mutex.Unlock()

	_ = p.handle.SetProperty("pause", mpv.FORMAT_FLAG, true)
	if timePos, err := p.getMpvTimePos(); err == nil {
		p.mutex.Lock()
		p.cachedTimePos = timePos
		p.mutex.Unlock()
	}

	p.mutex.Lock()
	p.state = types.Paused
	p.mutex.Unlock()
	select {
	case p.stateChan <- types.Paused:
	case <-time.After(time.Second * 2):
	}
}

// Resume 恢复播放
func (p *mpvPlayer) Resume() {
	p.mutex.Lock()
	if p.state != types.Paused && p.state != types.Stopped {
		p.mutex.Unlock()
		return
	}
	p.mutex.Unlock()

	_ = p.handle.SetProperty("pause", mpv.FORMAT_FLAG, false)
	p.mutex.Lock()
	p.state = types.Playing
	p.mutex.Unlock()
	select {
	case p.stateChan <- types.Playing:
	case <-time.After(time.Second * 2):
	}
}

// Stop 停止播放
func (p *mpvPlayer) Stop() {
	_ = p.handle.Command([]string{"stop"})
	p.mutex.Lock()
	p.stopTicker()
	p.state = types.Stopped
	p.mutex.Unlock()
	select {
	case p.stateChan <- types.Stopped:
	case <-time.After(time.Second * 2):
	}
}

// Toggle 切换播放/暂停状态
func (p *mpvPlayer) Toggle() {
	switch p.State() {
	case types.Paused, types.Stopped:
		p.Resume()
	case types.Playing:
		p.Pause()
	default:
		p.Resume()
	}
}

// Seek 跳转到指定时间
func (p *mpvPlayer) Seek(duration time.Duration) {
	p.mutex.Lock()
	if p.state != types.Playing && p.state != types.Paused {
		p.mutex.Unlock()
		return
	}
	p.mutex.Unlock()

	if err := p.handle.SetProperty("time-pos", mpv.FORMAT_DOUBLE, duration.Seconds()); err != nil {
		slog.Error("跳转命令发送失败", slogx.Error(err))
		return
	}

	// 更新缓存位置
	p.mutex.Lock()
	p.cachedTimePos = duration
	p.lastSyncTime = time.Now()
	p.mutex.Unlock()
}

// PassedTime 获取已播放时间（基于 MPV 的实际播放时间）
func (p *mpvPlayer) PassedTime() time.Duration {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// 如果不在播放状态，返回缓存位置
	if p.state != types.Playing {
		return p.cachedTimePos
	}

	// 如果最近2秒内同步过，使用缓存值加上估算的增量
	if time.Since(p.lastSyncTime) < 2*time.Second {
		elapsed := time.Since(p.lastSyncTime)
		return p.cachedTimePos + elapsed
	}

	// 否则返回缓存值（可能已经过时，但下一次 tick 会更新）
	return p.cachedTimePos
}

// PlayedTime 获取从播放开始到现在的时间
func (p *mpvPlayer) PlayedTime() time.Duration {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.playStartTime.IsZero() {
		return 0
	}
	return time.Since(p.playStartTime)
}

// TimeChan 获取时间更新通道
func (p *mpvPlayer) TimeChan() <-chan time.Duration {
	return p.timeChan
}

// State 获取当前状态
func (p *mpvPlayer) State() types.State {
	return p.state
}

// StateChan 获取状态更新通道
func (p *mpvPlayer) StateChan() <-chan types.State {
	return p.stateChan
}

// setState 设置状态并通知
func (p *mpvPlayer) setState(state types.State) {
	p.mutex.Lock()
	p.state = state
	p.mutex.Unlock()
	select {
	case p.stateChan <- state:
	case <-time.After(time.Second * 2):
	}
}

// Volume 获取当前音量
func (p *mpvPlayer) Volume() int {
	return p.volume
}

// SetVolume 设置音量
func (p *mpvPlayer) SetVolume(volume int) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if volume > 100 {
		volume = 100
	}
	if volume < 0 {
		volume = 0
	}

	p.volume = volume
	_ = p.handle.SetProperty("volume", mpv.FORMAT_INT64, int64(volume))
}

// UpVolume 增加音量
func (p *mpvPlayer) UpVolume() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.volume+5 >= 100 {
		p.volume = 100
	} else {
		p.volume += 5
	}

	_ = p.handle.SetProperty("volume", mpv.FORMAT_INT64, int64(p.volume))
}

// DownVolume 降低音量
func (p *mpvPlayer) DownVolume() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.volume-5 <= 0 {
		p.volume = 0
	} else {
		p.volume -= 5
	}

	_ = p.handle.SetProperty("volume", mpv.FORMAT_INT64, int64(p.volume))
}

// Close 关闭播放器
func (p *mpvPlayer) Close() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.stopTicker()

	// 停止事件监听
	if p.eventDone != nil {
		close(p.eventDone)
		p.eventDone = nil
	}

	if p.handle != nil {
		p.handle.TerminateDestroy()
		p.handle = nil
	}

	if p.close != nil {
		close(p.close)
		p.close = nil
	}
}
