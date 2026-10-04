//go:build windows

package sshpool

import (
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/crypto/ssh/agent"
)

// dialAgent connects to the Windows OpenSSH Authentication Agent (ssh-agent service).
func dialAgent() (agent.ExtendedAgent, func(), error) {
	timeout := 3 * time.Second
	conn, err := winio.DialPipe(`\.\pipe\openssh-ssh-agent`, &timeout)
	if err != nil {
		return nil, func() {}, err
	}
	return agent.NewClient(conn), func() { conn.Close() }, nil
}
