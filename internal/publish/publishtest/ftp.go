// Package publishtest は公開先への反映を試験するための、同じプロセスの中で動く FTP・SFTP サーバーを提供する。
// 試験のコードからだけ使う。
package publishtest

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// FTPServer は試験用の小さな FTP サーバー。Root のフォルダを / として見せる。
// 実際のレンタルサーバーと同じく、STOR は親フォルダが無ければ断り、MKD は1段ずつしか作らない。
type FTPServer struct {
	// Addr は待ち受けているアドレス（127.0.0.1:<port>）。
	Addr string
	// Root はサーバーの / に当たる手元のフォルダ。
	Root string
	// User・Password はログインを受け付ける組。
	User, Password string
	// TLS は AUTH TLS に使う設定。nil なら AUTH TLS を 502（未対応）で断る。
	TLS *tls.Config
	// FailStor が true を返すパス（/ から始まるサーバー上のパス）への STOR を 553 で断る。
	FailStor func(p string) bool

	ln net.Listener
	mu sync.Mutex
	// commands は受け取ったコマンド（引数を除く）。
	commands []string
}

// NewFTPServer は root を / として見せる FTP サーバーを起動する。試験の終わりに止める。
func NewFTPServer(t testing.TB, root string) *FTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ftp listen: %v", err)
	}
	s := &FTPServer{Addr: ln.Addr().String(), Root: root, User: "sorus", Password: "secret", ln: ln}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

// Port は待ち受けているポート番号。
func (s *FTPServer) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Commands は受け取ったコマンドの名前（USER・STOR など）を受け取った順に返す。
func (s *FTPServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *FTPServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

// local はサーバー上のパス p の手元のパス。Root の外は指さない。
func (s *FTPServer) local(p string) string {
	return filepath.Join(s.Root, filepath.FromSlash(path.Clean("/"+p)))
}

type ftpSession struct {
	s       *FTPServer
	conn    net.Conn
	r       *bufio.Reader
	user    string
	authed  bool
	protP   bool
	passive net.Listener
}

func (s *FTPServer) handle(c net.Conn) {
	sess := &ftpSession{s: s, conn: c, r: bufio.NewReader(c)}
	defer func() {
		if sess.passive != nil {
			sess.passive.Close()
		}
		sess.conn.Close()
	}()
	sess.reply("220 sorus test ftp")
	for {
		_ = sess.conn.SetDeadline(time.Now().Add(30 * time.Second))
		line, err := sess.r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg, _ := strings.Cut(line, " ")
		cmd = strings.ToUpper(cmd)
		s.mu.Lock()
		s.commands = append(s.commands, cmd)
		s.mu.Unlock()
		if !sess.exec(cmd, arg) {
			return
		}
	}
}

func (sess *ftpSession) reply(format string, args ...any) {
	fmt.Fprintf(sess.conn, format+"\r\n", args...)
}

// exec は1つのコマンドを処理する。接続を閉じるときは false を返す。
func (sess *ftpSession) exec(cmd, arg string) bool {
	s := sess.s
	switch cmd {
	case "AUTH":
		if s.TLS == nil {
			sess.reply("502 AUTH not supported")
			return true
		}
		sess.reply("234 AUTH TLS ok")
		tc := tls.Server(sess.conn, s.TLS)
		if err := tc.Handshake(); err != nil {
			return false
		}
		sess.conn, sess.r = tc, bufio.NewReader(tc)
	case "PBSZ":
		sess.reply("200 PBSZ=0")
	case "PROT":
		sess.protP = strings.EqualFold(arg, "P")
		sess.reply("200 PROT ok")
	case "USER":
		sess.user = arg
		sess.reply("331 password please")
	case "PASS":
		if sess.user == s.User && arg == s.Password {
			sess.authed = true
			sess.reply("230 logged in")
		} else {
			sess.reply("530 login incorrect")
		}
	case "QUIT":
		sess.reply("221 bye")
		return false
	case "FEAT":
		sess.reply("211-Features:\r\n EPSV\r\n PASV\r\n SIZE\r\n UTF8\r\n211 End")
	case "TYPE", "OPTS", "NOOP":
		sess.reply("200 ok")
	default:
		if !sess.authed {
			sess.reply("530 not logged in")
			return true
		}
		sess.fileCommand(cmd, arg)
	}
	return true
}

// fileCommand はログイン後のファイル操作のコマンドを処理する。
func (sess *ftpSession) fileCommand(cmd, arg string) {
	s := sess.s
	switch cmd {
	case "PWD":
		sess.reply(`257 "/"`)
	case "CWD":
		if info, err := os.Stat(s.local(arg)); err == nil && info.IsDir() {
			sess.reply("250 ok")
		} else {
			sess.reply("550 no such directory")
		}
	case "EPSV", "PASV":
		if sess.passive != nil {
			sess.passive.Close()
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			sess.reply("425 cannot open data connection")
			return
		}
		sess.passive = ln
		port := ln.Addr().(*net.TCPAddr).Port
		if cmd == "EPSV" {
			sess.reply("229 Entering Extended Passive Mode (|||%d|)", port)
		} else {
			sess.reply("227 Entering Passive Mode (127,0,0,1,%d,%d)", port/256, port%256)
		}
	case "STOR":
		p := s.local(arg)
		if s.FailStor != nil && s.FailStor(path.Clean("/"+arg)) {
			sess.closePassive()
			sess.reply("553 permission denied")
			return
		}
		if info, err := os.Stat(filepath.Dir(p)); err != nil || !info.IsDir() {
			sess.closePassive()
			sess.reply("553 no such directory")
			return
		}
		data, ok := sess.openData()
		if !ok {
			return
		}
		f, err := os.Create(p)
		if err != nil {
			data.Close()
			sess.reply("553 cannot create")
			return
		}
		_, cerr := io.Copy(f, data)
		data.Close()
		if err := f.Close(); err != nil || cerr != nil {
			sess.reply("451 write failed")
			return
		}
		sess.reply("226 transfer complete")
	case "NLST":
		dir := s.local(arg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			sess.closePassive()
			sess.reply("550 no such directory")
			return
		}
		data, ok := sess.openData()
		if !ok {
			return
		}
		for _, e := range entries {
			fmt.Fprintf(data, "%s\r\n", e.Name())
		}
		data.Close()
		sess.reply("226 transfer complete")
	case "SIZE":
		info, err := os.Stat(s.local(arg))
		if err != nil || info.IsDir() {
			sess.reply("550 no such file")
			return
		}
		sess.reply("213 %d", info.Size())
	case "DELE":
		p := s.local(arg)
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			sess.reply("550 no such file")
			return
		}
		if err := os.Remove(p); err != nil {
			sess.reply("550 cannot delete")
			return
		}
		sess.reply("250 deleted")
	case "MKD":
		if err := os.Mkdir(s.local(arg), 0o755); err != nil {
			sess.reply("550 cannot create directory")
			return
		}
		sess.reply(`257 "%s" created`, arg)
	case "RMD":
		if err := os.Remove(s.local(arg)); err != nil {
			sess.reply("550 cannot remove directory")
			return
		}
		sess.reply("250 removed")
	default:
		sess.reply("502 command not implemented")
	}
}

// openData は直前の EPSV・PASV で開いたデータ接続を受け付け、150 を返す。
func (sess *ftpSession) openData() (net.Conn, bool) {
	if sess.passive == nil {
		sess.reply("425 use EPSV first")
		return nil, false
	}
	_ = sess.passive.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	c, err := sess.passive.Accept()
	sess.closePassive()
	if err != nil {
		sess.reply("425 cannot open data connection")
		return nil, false
	}
	sess.reply("150 opening data connection")
	if sess.protP && sess.s.TLS != nil {
		return tls.Server(c, sess.s.TLS), true
	}
	return c, true
}

func (sess *ftpSession) closePassive() {
	if sess.passive != nil {
		sess.passive.Close()
		sess.passive = nil
	}
}
