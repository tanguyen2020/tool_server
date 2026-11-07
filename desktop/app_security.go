package main

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"serverdash/internal/security"
	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

// Security tab: sessions, SSH logins, authorized keys, the ufw firewall and the ports open to the network.

func (a *App) secHost(serverID string) (store.Server, sshHost, error) {
	srv, conn, err := a.server(serverID)
	if err != nil {
		return srv, sshHost{}, err
	}
	return srv, sshHost{conn: conn}, nil
}

func (a *App) SecurityOverview(serverID string) (security.Overview, error) {
	srv, h, err := a.secHost(serverID)
	if err != nil {
		return security.Overview{}, err
	}
	return security.ReadOverview(a.ctx, h, sshpool.DockerBin(srv))
}

// SecurityLogins sums up the SSH logins of the last days (failed attempts by address, recent logins).
func (a *App) SecurityLogins(serverID string, days int) (security.Logins, error) {
	_, h, err := a.secHost(serverID)
	if err != nil {
		return security.Logins{}, err
	}
	lg, err := security.ReadLogins(a.ctx, h, days)
	if err == nil && lg.AppKey != "" {
		a.appKey.Store(serverID, lg.AppKey)
	}
	return lg, err
}

// appKeyOf is the fingerprint of the key the app signs in with: from its own key, or from the SSH log.
func (a *App) appKeyOf(srv store.Server) string {
	if fp := sshpool.KeyFingerprint(srv); fp != "" {
		return fp
	}
	if v, ok := a.appKey.Load(srv.ID); ok {
		return v.(string)
	}
	return ""
}

func (a *App) SecurityKeys(serverID string) (security.Keys, error) {
	srv, h, err := a.secHost(serverID)
	if err != nil {
		return security.Keys{}, err
	}
	k, err := security.ReadKeys(a.ctx, h)
	k.AppKey = a.appKeyOf(srv)
	return k, err
}

// SecurityAddKey authorizes a public key for a user; with nu.Create the user is created first (keys only).
func (a *App) SecurityAddKey(serverID, user, line string, nu security.NewUser) (security.AuthKey, error) {
	_, h, err := a.secHost(serverID)
	if err != nil {
		return security.AuthKey{}, err
	}
	// The key is checked before the user is created, so a bad key leaves nothing behind.
	if _, err := security.CheckKey(line); err != nil {
		return security.AuthKey{}, err
	}
	if nu.Create {
		detail := user
		var extra []string
		if nu.Docker {
			extra = append(extra, "docker group")
		}
		if nu.Sudo {
			extra = append(extra, "sudo without a password")
		}
		if len(extra) > 0 {
			detail += " (" + strings.Join(extra, ", ") + ")"
		}
		err := security.CreateUser(a.ctx, h, user, nu)
		a.audit(serverID, "user create", detail, err)
		if err != nil {
			return security.AuthKey{}, err
		}
	}
	key, err := security.AddKey(a.ctx, h, user, line)
	a.audit(serverID, "ssh key add", fmt.Sprintf("%s: %s %s", user, key.Fingerprint, key.Comment), err)
	return key, err
}

// SecurityRemoveKey removes a key from a user's authorized_keys. The key the app signs in with is refused.
func (a *App) SecurityRemoveKey(serverID, user, path, fingerprint string) error {
	srv, h, err := a.secHost(serverID)
	if err != nil {
		return err
	}
	if user == srv.Username && fingerprint != "" && fingerprint == a.appKeyOf(srv) {
		return errors.New("this is the key Server Dashboard signs in with: removing it would lock the app out of this server")
	}
	err = security.RemoveKey(a.ctx, h, user, path, fingerprint)
	a.audit(serverID, "ssh key remove", fmt.Sprintf("%s: %s", user, fingerprint), err)
	return err
}

func (a *App) SecurityEndSession(serverID, id, tty, label string) error {
	_, h, err := a.secHost(serverID)
	if err != nil {
		return err
	}
	err = security.EndSession(a.ctx, h, id, tty)
	a.audit(serverID, "end session", label, err)
	return err
}

// ------------------------------------------------------------------ firewall (ufw)

// fwChange reads the firewall as it is now, so a change is checked against the real rules.
func (a *App) fwChange(serverID string) (sshHost, security.Overview, error) {
	srv, h, err := a.secHost(serverID)
	if err != nil {
		return h, security.Overview{}, err
	}
	o, err := security.ReadOverview(a.ctx, h, sshpool.DockerBin(srv))
	if err == nil && !o.Firewall.Readable {
		err = errors.New("changing the firewall needs root: connect as root, or allow this user to use sudo without a password")
	}
	return h, o, err
}

// FirewallEnable turns ufw on; the message says when a rule allowing SSH was added first.
func (a *App) FirewallEnable(serverID string) (string, error) {
	h, o, err := a.fwChange(serverID)
	if err != nil {
		return "", err
	}
	msg, err := security.Enable(a.ctx, h, o)
	a.audit(serverID, "firewall on", strings.TrimSpace("ufw "+msg), err)
	return msg, err
}

func (a *App) FirewallDisable(serverID string) error {
	h, _, err := a.fwChange(serverID)
	if err != nil {
		return err
	}
	err = security.Disable(a.ctx, h)
	a.audit(serverID, "firewall off", "ufw", err)
	return err
}

func (a *App) FirewallAddRule(serverID string, r security.NewRule) error {
	h, o, err := a.fwChange(serverID)
	if err != nil {
		return err
	}
	err = security.AddRule(a.ctx, h, o, r)
	a.audit(serverID, "firewall rule add", fmt.Sprintf("%s %s %s from %s", r.Action, r.Port, r.Proto, r.From), err)
	return err
}

func (a *App) FirewallDeleteRule(serverID, cmd string) error {
	h, o, err := a.fwChange(serverID)
	if err != nil {
		return err
	}
	err = security.DeleteRule(a.ctx, h, o, cmd)
	a.audit(serverID, "firewall rule delete", cmd, err)
	return err
}

// FirewallBlock blocks every connection from an address (a deny rule ahead of the others).
func (a *App) FirewallBlock(serverID, ip string, attempts int) error {
	if net.ParseIP(ip) == nil {
		return errors.New("invalid IP address")
	}
	h, o, err := a.fwChange(serverID)
	if err != nil {
		return err
	}
	comment := "Blocked by Server Dashboard"
	if attempts > 0 {
		comment = fmt.Sprintf("Blocked by Server Dashboard after %d failed SSH logins", attempts)
	}
	err = security.AddRule(a.ctx, h, o, security.NewRule{Action: "deny", From: ip, Comment: comment})
	a.audit(serverID, "firewall block", ip, err)
	return err
}
