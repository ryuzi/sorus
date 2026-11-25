package publishtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTPServer は試験用の SSH・SFTP サーバー。手元のファイルシステムをそのまま見せるため、試験では
// t.TempDir() の下の絶対パスをアップロード先にする。
type SFTPServer struct {
	// Addr は待ち受けているアドレス（127.0.0.1:<port>）。
	Addr string
	// User・Password はログインを受け付ける組。
	User, Password string

	ln net.Listener
	mu sync.Mutex
	// hostKey はサーバーの鍵。SetHostKey で差し替えると、なりすましの試験になる。
	hostKey ssh.Signer
}

// NewHostKey は試験用の SSH のサーバー鍵（Ed25519）を作る。
func NewHostKey(t testing.TB) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// NewSFTPServer は hostKey を使う SFTP サーバーを起動する。試験の終わりに止める。
func NewSFTPServer(t testing.TB, hostKey ssh.Signer) *SFTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sftp listen: %v", err)
	}
	s := &SFTPServer{Addr: ln.Addr().String(), User: "sorus", Password: "secret", ln: ln, hostKey: hostKey}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

// Port は待ち受けているポート番号。
func (s *SFTPServer) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// SetHostKey は以後の接続で使うサーバー鍵を差し替える。
func (s *SFTPServer) SetHostKey(k ssh.Signer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostKey = k
}

// Fingerprint は今のサーバー鍵の指紋（ssh.FingerprintSHA256 の形式）。
func (s *SFTPServer) Fingerprint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ssh.FingerprintSHA256(s.hostKey.PublicKey())
}

func (s *SFTPServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *SFTPServer) handle(c net.Conn) {
	defer c.Close()
	conf := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if meta.User() == s.User && string(pw) == s.Password {
				return nil, nil
			}
			return nil, errLoginIncorrect
		},
	}
	s.mu.Lock()
	conf.AddHostKey(s.hostKey)
	s.mu.Unlock()
	sc, chans, reqs, err := ssh.NewServerConn(c, conf)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, in, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range in {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
				if !ok {
					continue
				}
				srv, err := sftp.NewServer(ch)
				if err != nil {
					ch.Close()
					return
				}
				_ = srv.Serve()
				srv.Close()
				ch.Close()
				return
			}
		}()
	}
}

var errLoginIncorrect = errors.New("login incorrect")
