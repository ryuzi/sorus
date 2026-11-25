package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTPConfig はレンタルサーバーへ SFTP（SSH）で接続する設定。
type SFTPConfig struct {
	Host string
	// Port は 0 なら 22。
	Port int
	User string
	// Password はパスワード。PrivateKey にパスフレーズがあるときは、そのパスフレーズにも使う。
	Password string
	// PrivateKey は SSH の秘密鍵（PEM・OpenSSH 形式）。空ならパスワードだけで接続する。
	PrivateKey string
	// Root はアップロード先のフォルダ（例: /home/user/public_html）。CleanRoot で検査する。
	Root string
	// Timeout は接続の制限時間。0 なら DefaultTimeout。
	Timeout time.Duration
	// KnownHostKey は前回接続したときに覚えた接続先の鍵の指紋（ssh.FingerprintSHA256 の形式）。空なら初めての接続として
	// どの鍵も受け入れ（trust on first use）、その指紋を SFTPRemote.HostKey で返す。覚えている指紋と違えば
	// HostKeyChangedError で接続をやめる。
	KnownHostKey string
}

// HostKeyChangedError は接続先の鍵が、前回接続したときに覚えた鍵と違うことを示す。別のサーバーになりすまされている
// おそれがあるため、利用者が確かめて信頼し直すまで送らない。
type HostKeyChangedError struct {
	// Known は覚えていた指紋、Got は今回の接続先の指紋。
	Known, Got string
}

func (e *HostKeyChangedError) Error() string {
	return fmt.Sprintf("publish: sftp host key changed (known %s, got %s)", e.Known, e.Got)
}

// SFTPRemote は SFTP の公開先への接続。
type SFTPRemote struct {
	ssh    *ssh.Client
	client *sftp.Client
	root   string
	// hostKey は接続先の鍵の指紋。
	hostKey string
	made    map[string]bool
	stop    func() bool
}

// DialSFTP は cfg のサーバーに接続してログインする。ctx が終わると接続を閉じる（送信中の処理も止まる）。
func DialSFTP(ctx context.Context, cfg SFTPConfig) (*SFTPRemote, error) {
	root, err := CleanRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return nil, errors.New("publish: sftp host is empty")
	}
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	auth, err := sftpAuth(cfg)
	if err != nil {
		return nil, err
	}

	var got string
	var changed *HostKeyChangedError
	conf := &ssh.ClientConfig{
		User: cfg.User,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got = ssh.FingerprintSHA256(key)
			if cfg.KnownHostKey != "" && got != cfg.KnownHostKey {
				changed = &HostKeyChangedError{Known: cfg.KnownHostKey, Got: got}
				return changed
			}
			return nil
		},
		Timeout: timeout,
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("publish: connect %s: %w", addr, err)
	}
	// 鍵の交換とログインが終わらないまま待ち続けないよう、制限時間を付ける。
	_ = nc.SetDeadline(time.Now().Add(timeout))
	sc, chans, reqs, err := ssh.NewClientConn(nc, addr, conf)
	if err != nil {
		nc.Close()
		if changed != nil {
			return nil, changed
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("%w: %v", ErrAuth, err)
		}
		return nil, fmt.Errorf("publish: ssh handshake %s: %w", addr, err)
	}
	_ = nc.SetDeadline(time.Time{})
	sshClient := ssh.NewClient(sc, chans, reqs)
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("publish: start sftp: %w", err)
	}
	r := &SFTPRemote{ssh: sshClient, client: client, root: root, hostKey: got, made: map[string]bool{}}
	r.stop = context.AfterFunc(ctx, func() { _ = sshClient.Close() })
	return r, nil
}

// sftpAuth は cfg のパスワード・秘密鍵からログインの方法を作る。
func sftpAuth(cfg SFTPConfig) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if key := strings.TrimSpace(cfg.PrivateKey); key != "" {
		signer, err := ssh.ParsePrivateKey([]byte(key))
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(cfg.Password))
		}
		if err != nil {
			return nil, fmt.Errorf("%w: private key: %v", ErrAuth, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		pw := cfg.Password
		methods = append(methods, ssh.Password(pw),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = pw
				}
				return answers, nil
			}))
	}
	return methods, nil
}

// HostKey は接続先の鍵の指紋（ssh.FingerprintSHA256 の形式）。初めての接続では、これを覚えて次からの KnownHostKey にする。
func (r *SFTPRemote) HostKey() string { return r.hostKey }

func (r *SFTPRemote) full(rel string) string { return path.Join(r.root, rel) }

func (r *SFTPRemote) Put(ctx context.Context, rel string, body io.Reader, size int64) error {
	p := r.full(rel)
	if dir := path.Dir(p); !r.made[dir] {
		if err := r.client.MkdirAll(dir); err != nil {
			return err
		}
		r.made[dir] = true
	}
	f, err := r.client.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.ReadFrom(body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *SFTPRemote) Delete(ctx context.Context, rel string) error {
	if err := r.client.Remove(r.full(rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r *SFTPRemote) RemoveDir(ctx context.Context, rel string) error {
	return r.client.RemoveDirectory(r.full(rel))
}

func (r *SFTPRemote) CountEntries(ctx context.Context) (int, error) {
	entries, err := r.client.ReadDir(r.root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

func (r *SFTPRemote) Notes() []string { return nil }

func (r *SFTPRemote) Close() error {
	r.stop()
	err := r.client.Close()
	return errors.Join(err, r.ssh.Close())
}
