// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux freebsd openbsd

package dns

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"inet.af/netaddr"
	"tailscale.com/atomicfile"
)

const (
	tsConf     = "/etc/resolv.tailscale.conf"
	backupConf = "/etc/resolv.pre-tailscale-backup.conf"
	resolvConf = "/etc/resolv.conf"
)

// writeResolvConf writes DNS configuration in resolv.conf format to the given writer.
func writeResolvConf(w io.Writer, servers []netaddr.IP, domains []string) {
	io.WriteString(w, "# resolv.conf(5) file generated by tailscale\n")
	io.WriteString(w, "# DO NOT EDIT THIS FILE BY HAND -- CHANGES WILL BE OVERWRITTEN\n\n")
	for _, ns := range servers {
		io.WriteString(w, "nameserver ")
		io.WriteString(w, ns.String())
		io.WriteString(w, "\n")
	}
	if len(domains) > 0 {
		io.WriteString(w, "search")
		for _, domain := range domains {
			io.WriteString(w, " ")
			io.WriteString(w, domain)
		}
		io.WriteString(w, "\n")
	}
}

// readResolvConf reads DNS configuration from /etc/resolv.conf.
func readResolvConf() (Config, error) {
	var config Config

	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return config, err
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "nameserver") {
			nameserver := strings.TrimPrefix(line, "nameserver")
			nameserver = strings.TrimSpace(nameserver)
			ip, err := netaddr.ParseIP(nameserver)
			if err != nil {
				return config, err
			}
			config.Nameservers = append(config.Nameservers, ip)
			continue
		}

		if strings.HasPrefix(line, "search") {
			domain := strings.TrimPrefix(line, "search")
			domain = strings.TrimSpace(domain)
			config.Domains = append(config.Domains, domain)
			continue
		}
	}

	return config, nil
}

// isResolvedRunning reports whether systemd-resolved is running on the system,
// even if it is not managing the system DNS settings.
func isResolvedRunning() bool {
	if runtime.GOOS != "linux" {
		return false
	}

	// systemd-resolved is never installed without systemd.
	_, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}

	// is-active exits with code 3 if the service is not active.
	err = exec.Command("systemctl", "is-active", "systemd-resolved.service").Run()

	return err == nil
}

// directManager is a managerImpl which replaces /etc/resolv.conf with a file
// generated from the given configuration, creating a backup of its old state.
//
// This way of configuring DNS is precarious, since it does not react
// to the disappearance of the Tailscale interface.
// The caller must call Down before program shutdown
// or as cleanup if the program terminates unexpectedly.
type directManager struct{}

func newDirectManager() managerImpl {
	return directManager{}
}

// Up implements managerImpl.
func (m directManager) Up(config Config) error {
	// Write the tsConf file.
	buf := new(bytes.Buffer)
	writeResolvConf(buf, config.Nameservers, config.Domains)
	if err := atomicfile.WriteFile(tsConf, buf.Bytes(), 0644); err != nil {
		return err
	}

	if linkPath, err := os.Readlink(resolvConf); err != nil {
		// Remove any old backup that may exist.
		os.Remove(backupConf)

		// Backup the existing /etc/resolv.conf file.
		contents, err := ioutil.ReadFile(resolvConf)
		// If the original did not exist, still back up an empty file.
		// The presence of a backup file is the way we know that Up ran.
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := atomicfile.WriteFile(backupConf, contents, 0644); err != nil {
			return err
		}
	} else if linkPath != tsConf {
		// Backup the existing symlink.
		os.Remove(backupConf)
		if err := os.Symlink(linkPath, backupConf); err != nil {
			return err
		}
	} else {
		// Nothing to do, resolvConf already points to tsConf.
		return nil
	}

	os.Remove(resolvConf)
	if err := os.Symlink(tsConf, resolvConf); err != nil {
		return err
	}

	if isResolvedRunning() {
		exec.Command("systemctl", "restart", "systemd-resolved.service").Run() // Best-effort.
	}

	return nil
}

// Down implements managerImpl.
func (m directManager) Down() error {
	if _, err := os.Stat(backupConf); err != nil {
		// If the backup file does not exist, then Up never ran successfully.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if ln, err := os.Readlink(resolvConf); err != nil {
		return err
	} else if ln != tsConf {
		return fmt.Errorf("resolv.conf is not a symlink to %s", tsConf)
	}
	if err := os.Rename(backupConf, resolvConf); err != nil {
		return err
	}
	os.Remove(tsConf)

	if isResolvedRunning() {
		exec.Command("systemctl", "restart", "systemd-resolved.service").Run() // Best-effort.
	}

	return nil
}
