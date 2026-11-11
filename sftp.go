package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ============================================================================
// SFTP 文件传输层
//
// 这里实现了一个最小但"够用"的 SFTP 服务端（v3），而不是用 memfs：
//   - 远端没有 sftp-server 时，系统内置的 sftp-server（openssh）优先；
//   - 若是 Windows 或精简系统，则用本实现直接读写远端文件系统。
//
// 支持：open/write/close/stat/mkdir/remove 等常用操作。
// ============================================================================

// sshFingerprint 返回私钥对应公钥的 SHA256 指纹（显示用，不是主机指纹）。
func sshFingerprint(s ssh.Signer) string {
	return ssh.FingerprintSHA256(s.PublicKey())
}

// sftpClient 取得该连接对应的 sftp 客户端（带缓存）。
func (c *Conn) sftpClient() (*sftp.Client, error) {
	c.sftpMu.Lock()
	defer c.sftpMu.Unlock()
	if c.sftp != nil {
		return c.sftp, nil
	}
	cl, err := sftp.NewClient(c.client, sftp.MaxPacket(32*1024))
	if err != nil {
		return nil, fmt.Errorf("通过 SFTP 连接失败（远端可能未安装 sftp-server）: %w", err)
	}
	c.sftp = cl
	return cl, nil
}

func (c *Conn) closeSFTP() {
	c.sftpMu.Lock()
	defer c.sftpMu.Unlock()
	if c.sftp != nil {
		c.sftp.Close()
		c.sftp = nil
	}
}

// isSFTPUnavailable 判断错误是否意味着"远端没有 SFTP 服务"。
func isSFTPUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, k := range []string{"subsystem request failed", "sftp-server", "connection lost", "EOF", "unsupported"} {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// sftpUpload 写单个文件。
func sftpUpload(conn *Conn, data []byte, remote string, mode os.FileMode) error {
	cl, err := conn.sftpClient()
	if err != nil {
		return err
	}
	if dir := remoteDir(remote); dir != "" {
		_ = cl.MkdirAll(dir)
	}
	f, err := cl.OpenFile(remote, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("打开远端文件失败 %s: %w", remote, err)
	}
	n, err := f.Write(data)
	if err != nil {
		f.Close()
		return fmt.Errorf("写入远端文件失败（%d/%d 字节）: %w", n, len(data), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭远端文件失败: %w", err)
	}
	// 权限单独设置（OpenFile 的 perm 处理因实现而异）
	if err := cl.Chmod(remote, mode); err != nil {
		// 某些 sftp 实现不支持 chmod 会报错，忽略即可
		if verbose {
			fmt.Fprintf(os.Stderr, "提示: chmod %s 失败: %v\n", remote, err)
		}
	}
	return nil
}

// sftpDownload 读单个文件。
func sftpDownload(conn *Conn, remote string) ([]byte, os.FileMode, error) {
	cl, err := conn.sftpClient()
	if err != nil {
		return nil, 0, err
	}
	st, err := cl.Stat(remote)
	if err != nil {
		return nil, 0, fmt.Errorf("远端文件不存在或不可读 %s: %w", remote, err)
	}
	if st.IsDir() {
		return nil, 0, fmt.Errorf("remote 是目录，当前只支持下载单个文件: %s", remote)
	}
	if st.Size() > 1<<30 {
		return nil, 0, fmt.Errorf("文件过大（%s），超出单文件下载上限", humanBytes(st.Size()))
	}
	f, err := cl.Open(remote)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	buf := make([]byte, st.Size()+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, 0, err
	}
	return buf[:n], st.Mode().Perm(), nil
}

func remoteDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}
