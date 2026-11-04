// 后台管理页（单文件 HTML，无构建步骤）：/admin
//
// 与 H5 应用（web/dist）完全独立 —— 不参与 npm 构建，只经 go:embed 打进
// `chonkpilot-remote-server.exe`。页面自身用 /api/login 登录，再按 admin 角色
// 调用 /api/admin/* 完成账号维护与设备接入查看。
package main

import (
	_ "embed"
	"errors"
)

//go:embed adminweb/admin.html
var adminHTML []byte

// adminPageHTML 返回后台管理页内容（空 = 构建异常）。
func adminPageHTML() ([]byte, error) {
	if len(adminHTML) == 0 {
		return nil, errors.New("admin page not embedded")
	}
	return adminHTML, nil
}
