package listeners // import "github.com/docker/docker/daemon/listeners"

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/docker/docker/pkg/homedir"
	"github.com/docker/go-connections/sockets"
	"github.com/sirupsen/logrus"
)

// Init creates new listeners for the server.
// TODO: Clean up the fact that socketGroup and tlsConfig aren't always used.
func Init(proto, addr, socketGroup string, tlsConfig *tls.Config) ([]net.Listener, error) {
	ls := []net.Listener{}

	switch proto {
	case "fd":
		// ExecStart=/usr/bin/dockerd -H fd://
		// systemd启动

		// netstat |grep docker 发现两种方式都是unix 监听
		fds, err := listenFD(addr, tlsConfig)
		if err != nil {
			return nil, err
		}
		ls = append(ls, fds...)
	case "tcp":
		l, err := sockets.NewTCPSocket(addr, tlsConfig)
		if err != nil {
			return nil, err
		}
		ls = append(ls, l)
	case "unix":
		// dockerd启动，此时需要自己创建并启动docker.sock监听
		gid, err := lookupGID(socketGroup)
		if err != nil {
			if socketGroup != "" {
				if socketGroup != defaultSocketGroup {
					return nil, err
				}
				logrus.Warnf("could not change group %s to %s: %v", addr, defaultSocketGroup, err)
			}
			gid = os.Getgid()
		}

		// 直接listen unix文件会自动创建unix监听文件，该文件存在则报错
		l, err := sockets.NewUnixSocket(addr, gid)
		if err != nil {
			return nil, fmt.Errorf("can't create unix socket %s: %v", addr, err)
		}
		if _, err := homedir.StickRuntimeDirContents([]string{addr}); err != nil {
			// StickRuntimeDirContents returns nil error if XDG_RUNTIME_DIR is just unset
			logrus.WithError(err).Warnf("cannot set sticky bit on socket %s under XDG_RUNTIME_DIR", addr)
		}
		ls = append(ls, l)
	default:
		return nil, fmt.Errorf("invalid protocol format: %q", proto)
	}

	return ls, nil
}

// listenFD returns the specified socket activated files as a slice of
// net.Listeners or all of the activated files if "*" is given
// listenFD返回指定套接字激活的文件作为net.listeners的片段，如果给定“*”，则返回所有激活的文件。
//
func listenFD(addr string, tlsConfig *tls.Config) ([]net.Listener, error) {
	var (
		err       error
		listeners []net.Listener
	)
	// socket activation
	if tlsConfig != nil {
		listeners, err = activation.TLSListeners(tlsConfig)
	} else {
		listeners, err = activation.Listeners()
	}
	if err != nil {
		return nil, err
	}

	if len(listeners) == 0 {
		return nil, fmt.Errorf("no sockets found via socket activation: make sure the service was started by systemd")
	}

	// default to all fds just like unix:// and tcp://
	if addr == "" || addr == "*" {
		return listeners, nil
	}

	fdNum, err := strconv.Atoi(addr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse systemd fd address: should be a number: %v", addr)
	}
	fdOffset := fdNum - 3
	if len(listeners) < fdOffset+1 {
		return nil, fmt.Errorf("too few socket activated files passed in by systemd")
	}
	if listeners[fdOffset] == nil {
		return nil, fmt.Errorf("failed to listen on systemd activated file: fd %d", fdOffset+3)
	}
	for i, ls := range listeners {
		if i == fdOffset || ls == nil {
			continue
		}
		if err := ls.Close(); err != nil {
			return nil, fmt.Errorf("failed to close systemd activated file: fd %d: %v", fdOffset+3, err)
		}
	}
	return []net.Listener{listeners[fdOffset]}, nil
}
