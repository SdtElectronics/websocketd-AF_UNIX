package libwebsocketd

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var ScriptNotFoundError = errors.New("script not found")

// WebsocketdHandler is a single request information and processing structure, it handles WS requests out of all that daemon can handle (static, cgi, devconsole)
type WebsocketdHandler struct {
	server *WebsocketdServer

	Id string
	*RemoteInfo
	*URLInfo // TODO: I cannot find where it's used except in one single place as URLInfo.FilePath
	Env      []string

	command string
}

// NewWebsocketdHandler constructs the struct and parses all required things in it...
func NewWebsocketdHandler(s *WebsocketdServer, req *http.Request, log *LogScope) (wsh *WebsocketdHandler, err error) {
	wsh = &WebsocketdHandler{server: s, Id: generateId()}
	log.Associate("id", wsh.Id)

	wsh.RemoteInfo, err = GetRemoteInfo(req, s.Config)
	if err != nil {
		log.Error("session", "Could not understand remote address '%s': %s", req.RemoteAddr, err)
		return nil, err
	}
	log.Associate("remote", wsh.RemoteInfo.Host)

	wsh.URLInfo, err = GetURLInfo(req.URL.Path, s.Config)
	if err != nil {
		log.Access("session", "NOT FOUND: %s", err)
		return nil, err
	}

	wsh.command = s.Config.CommandName
	if s.Config.UsingScriptDir {
		wsh.command = wsh.URLInfo.FilePath
	}
	log.Associate("command", wsh.command)

	wsh.Env = createEnv(wsh, req, log)

	return wsh, nil
}

func (wsh *WebsocketdHandler) accept(ws *websocket.Conn, log *LogScope) {
	defer ws.Close()

	log.Access("session", "CONNECT")
	defer log.Access("session", "DISCONNECT")

	binary := wsh.server.Config.Binary

	wsEndpoint := NewWebSocketEndpoint(ws, binary, log)

	if wsh.server.Config.UnixSocket {
		cmd := exec.Command(wsh.command, wsh.server.Config.CommandArgs...)
		cmd.Env = wsh.Env

		if err := cmd.Start(); err != nil {
			log.Error("process", "Could not launch process %s %s (%s)", wsh.command, strings.Join(wsh.server.Config.CommandArgs, " "), err)
			return
		}

		wsh.server.unixSocketListener.SetDeadline(time.Now().Add(10*time.Second))
		conn, err := wsh.server.unixSocketListener.AcceptUnix()

		if err != nil {
			log.Error("process", "accept error: %s", err)
			return
		}

		log.Associate("pid", strconv.Itoa(cmd.Process.Pid))

		procEndpoint := NewDomainEndpoint(cmd, conn, log)

		if cms := wsh.server.Config.CloseMs; cms != 0 {
			procEndpoint.closetime += time.Duration(cms) * time.Millisecond
		}

		PipeEndpoints(procEndpoint, wsEndpoint)
	} else {
		launched, err := launchCmd(wsh.command, wsh.server.Config.CommandArgs, wsh.Env)
		if err != nil {
			log.Error("process", "Could not launch process %s %s (%s)", wsh.command, strings.Join(wsh.server.Config.CommandArgs, " "), err)
			return
		}

		log.Associate("pid", strconv.Itoa(launched.cmd.Process.Pid))

		procEndpoint := NewProcessEndpoint(launched, binary, log)

		if cms := wsh.server.Config.CloseMs; cms != 0 {
			procEndpoint.closetime += time.Duration(cms) * time.Millisecond
		}

		PipeEndpoints(procEndpoint, wsEndpoint)
	}
}

// RemoteInfo holds information about remote http client
type RemoteInfo struct {
	Addr, Host, Port string
}

// GetRemoteInfo creates RemoteInfo structure and fills its fields appropriately
func GetRemoteInfo(req *http.Request, config *Config) (*RemoteInfo, error) {
	addr, port, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return nil, err
	}

	if config.RemoteHeader != "" {
		addr = req.Header.Get(config.RemoteHeader)
	}

	var host string
	if config.ReverseLookup {
		hosts, err := net.LookupAddr(addr)
		if err != nil || len(hosts) == 0 {
			host = addr
		} else {
			host = hosts[0]
		}
	} else {
		host = addr
	}

	return &RemoteInfo{Addr: addr, Host: host, Port: port}, nil
}

// URLInfo - structure carrying information about current request and it's mapping to filesystem
type URLInfo struct {
	ScriptPath string
	PathInfo   string
	FilePath   string
}

// GetURLInfo is a function that parses path and provides URL info according to libwebsocketd.Config fields
func GetURLInfo(path string, config *Config) (*URLInfo, error) {
	if !config.UsingScriptDir {
		return &URLInfo{"/", path, ""}, nil
	}

	parts := strings.Split(path[1:], "/")
	urlInfo := &URLInfo{}

	for i, part := range parts {
		urlInfo.ScriptPath = strings.Join([]string{urlInfo.ScriptPath, part}, "/")
		urlInfo.FilePath = filepath.Join(config.ScriptDir, urlInfo.ScriptPath)
		isLastPart := i == len(parts)-1
		statInfo, err := os.Stat(urlInfo.FilePath)

		// not a valid path
		if err != nil {
			return nil, ScriptNotFoundError
		}

		// at the end of url but is a dir
		if isLastPart && statInfo.IsDir() {
			return nil, ScriptNotFoundError
		}

		// we've hit a dir, carry on looking
		if statInfo.IsDir() {
			continue
		}

		// no extra args
		if isLastPart {
			return urlInfo, nil
		}

		// build path info from extra parts of url
		urlInfo.PathInfo = "/" + strings.Join(parts[i+1:], "/")
		return urlInfo, nil
	}
	panic(fmt.Sprintf("GetURLInfo cannot parse path %#v", path))
}

func generateId() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}
