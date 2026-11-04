package main

import "errors"

// Notifier 是系统通知的抽象（测试注入 fake，不真弹）。
type Notifier interface {
	Notify(title, body, clickURL string) error
}

// ErrNotifyUnsupported 表示当前平台不提供系统通知。
var ErrNotifyUnsupported = errors.New("系统通知在当前平台不可用")

// notifierFunc 把函数适配为 Notifier。
type notifierFunc func(title, body, clickURL string) error

func (f notifierFunc) Notify(title, body, clickURL string) error { return f(title, body, clickURL) }
