// 指纹校验单测（#31）：匹配/不匹配/格式错/无来源，不需要真 sshd。
package mysqlsrc

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lazygophers/lazydb/internal/source"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var fakeAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}

func fp(k ssh.PublicKey) string {
	sum := sha256.Sum256(k.Marshal())
	return "SHA256:" + fpBase64(sum[:])
}

func TestHostKeyFingerprintMatch(t *testing.T) {
	k := testHostKey(t)
	cb, err := hostKeyCallback(&source.SSHConfig{Host: "bastion", Port: 22, HostKeySHA256: fp(k)})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("bastion:22", nil, k); err != nil {
		t.Fatalf("matching fingerprint rejected: %v", err)
	}
	// 前缀可省
	cb, err = hostKeyCallback(&source.SSHConfig{Host: "b", Port: 22,
		HostKeySHA256: strings.TrimPrefix(fp(k), "SHA256:")})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("b:22", nil, k); err != nil {
		t.Fatalf("fingerprint without prefix rejected: %v", err)
	}
}

func TestHostKeyFingerprintMismatchMentionsHost(t *testing.T) {
	k := testHostKey(t)
	other := testHostKey(t)
	cb, err := hostKeyCallback(&source.SSHConfig{Host: "bastion", Port: 2222, HostKeySHA256: fp(other)})
	if err != nil {
		t.Fatal(err)
	}
	err = cb("bastion:2222", fakeAddr, k)
	if err == nil || !strings.Contains(err.Error(), "bastion:2222") || !strings.Contains(err.Error(), "SHA256:") {
		t.Fatalf("err = %v, want host + fingerprints", err)
	}
}

func TestHostKeyBadFingerprintFormat(t *testing.T) {
	for _, bad := range []string{"SHA256:!!!notbase64", "SHA256:AAAA", "md5:f0:0d"} {
		if _, err := hostKeyCallback(&source.SSHConfig{Host: "b", Port: 22, HostKeySHA256: bad}); err == nil {
			t.Fatalf("bad fingerprint %q accepted", bad)
		}
	}
}

// 无显式指纹时走 known_hosts：无文件 = 可读拒连，不静默放行。
func TestHostKeyNoSourceRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := os.UserHomeDir(); err != nil {
		t.Skip("no home dir")
	}
	_, err := hostKeyCallback(&source.SSHConfig{Host: "bastion", Port: 22})
	if err == nil || !strings.Contains(err.Error(), "bastion:22") || !strings.Contains(err.Error(), "ssh-keyscan") {
		t.Fatalf("err = %v, want readable refusal with host", err)
	}
}

// known_hosts 有正确条目则放行；条目错误则拒绝（报错含主机名）。
func TestHostKeyKnownHosts(t *testing.T) {
	k := testHostKey(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{"[bastion]:2222"}, k)
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := hostKeyCallback(&source.SSHConfig{Host: "bastion", Port: 2222})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("bastion:2222", fakeAddr, k); err != nil {
		t.Fatalf("known_hosts entry rejected: %v", err)
	}
	other := testHostKey(t)
	err = cb("bastion:2222", fakeAddr, other)
	if err == nil || !strings.Contains(err.Error(), "bastion:2222") {
		t.Fatalf("err = %v, want mismatch refusal with host", err)
	}
}
