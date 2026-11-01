// testsrv is a local SSH+SFTP server used only by the e2e test harness. It
// accepts tester/pw123, echoes terminal input prefixed with "ECHO:", prints
// TEST-BANNER on connect, and serves an SFTP subsystem rooted at the temp
// directory printed on startup. It prints "READY <port> <root>" on stdout.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

func main() {
	root, err := os.MkdirTemp("", "termhive-e2e-")
	if err != nil {
		panic(err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	signer, err := gossh.NewSignerFromKey(privateKey)
	if err != nil {
		panic(err)
	}
	server := &ssh.Server{
		Handler: func(session ssh.Session) {
			// exec requests answer simple commands so the e2e harness can
			// exercise the sidecar's probes (shell-kind detection for the
			// OSC 7 follow hook) and the MCP exec tools.
			if session.RawCommand() != "" {
				command := session.RawCommand()
				switch {
				case strings.HasPrefix(command, "uname"):
					fmt.Fprintln(session, "Linux testvm")
				default:
					fmt.Fprintln(session, "EXEC:"+command)
				}
				session.Exit(0)
				return
			}
			fmt.Fprint(session, "TEST-BANNER\r\n")
			buffer := make([]byte, 1024)
			for {
				count, err := session.Read(buffer)
				if count > 0 {
					session.Write(append([]byte("ECHO:"), buffer[:count]...))
				}
				if err != nil {
					return
				}
			}
		},
		PasswordHandler: func(ctx ssh.Context, password string) bool {
			return ctx.User() == "tester" && password == "pw123"
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(session ssh.Session) {
				server, err := sftp.NewServer(session, sftp.WithServerWorkingDirectory(root))
				if err != nil {
					io.WriteString(session.Stderr(), err.Error())
					return
				}
				if err := server.Serve(); err != nil && err != io.EOF {
					fmt.Fprintln(os.Stderr, "sftp server:", err)
				}
				server.Close()
			},
		},
		HostSigners: []ssh.Signer{signer},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Printf("READY %d %s\n", listener.Addr().(*net.TCPAddr).Port, root)
	if err := server.Serve(listener); err != nil {
		panic(err)
	}
}
