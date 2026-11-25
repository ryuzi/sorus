package desktop

import (
	"os"
	"os/user"
	"strings"
)

// OSUsername は OS にログインしている利用者のユーザー名を返す（「はじめに」画面のユーザー名の初期値）。
// Windows の「コンピューター名\ユーザー名」はユーザー名の部分だけを返す。取得できなければ空を返す。
// sorus のユーザー名に使える形式かどうかは呼び出し側で確かめる。
func OSUsername() string {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	if name == "" {
		name = os.Getenv("USERNAME")
	}
	if i := strings.LastIndex(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSpace(name)
}
