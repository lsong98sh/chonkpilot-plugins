//go:build !windows

package main

// newSystemNotifier 非 Windows 平台不提供系统通知（返回 ErrNotifyUnsupported）。
func newSystemNotifier(logf func(format string, args ...any), fallback func() string) Notifier {
	return notifierFunc(func(title, body, clickURL string) error {
		if logf != nil {
			logf("crontab: 跳过系统通知（非 Windows）：%s", title)
		}
		return ErrNotifyUnsupported
	})
}
