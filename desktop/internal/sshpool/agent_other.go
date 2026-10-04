//go:build !windows

package sshpool

import (
	"errors"
	"net"
	"os"

	"golang.org/x/crypto/ssh/agent"
)

func dialAgent() (agent.ExtendedAgent, func(), error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, func() {}, errors.New("SSH_AUTH_SOCK is not set")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, func() {}, err
	}
	return agent.NewClient(conn), func() { conn.Close() }, nil
}
